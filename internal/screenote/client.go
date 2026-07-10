package screenote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL     *url.URL
	bearerToken string
	httpClient  *http.Client
}

const defaultHTTPTimeout = 30 * time.Second

type Error struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return http.StatusText(e.StatusCode)
}

func NewClient(baseURL, bearerToken string, httpClient *http.Client) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("base url is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("base url must include scheme and host")
	}
	httpClient = httpClientOrDefault(httpClient)
	return &Client{baseURL: parsed, bearerToken: bearerToken, httpClient: httpClient}, nil
}

func httpClientOrDefault(httpClient *http.Client) *http.Client {
	if httpClient == nil {
		return &http.Client{Timeout: defaultHTTPTimeout}
	}
	return httpClient
}

func (c *Client) Projects(ctx context.Context) (json.RawMessage, ProjectsResponse, error) {
	var out ProjectsResponse
	raw, err := c.doJSON(ctx, http.MethodGet, "/api/v1/projects", nil, nil, &out, nil)
	return raw, out, err
}

func (c *Client) Pages(ctx context.Context, project string) (json.RawMessage, error) {
	return c.doJSON(ctx, http.MethodGet, "/api/v1/projects/"+url.PathEscape(project)+"/pages", nil, nil, nil, nil)
}

func (c *Client) Screenshots(ctx context.Context, project string, query url.Values) (json.RawMessage, ScreenshotsResponse, error) {
	var out ScreenshotsResponse
	raw, err := c.doJSON(ctx, http.MethodGet, "/api/v1/projects/"+url.PathEscape(project)+"/screenshots", query, nil, &out, nil)
	return raw, out, err
}

func (c *Client) CreateScreenshot(ctx context.Context, project, title, pageValue, filename, contentType string, r io.Reader) (json.RawMessage, error) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if filename == "" || filename == "-" {
		filename = "stdin"
	}

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	headers := map[string]string{"Content-Type": writer.FormDataContentType()}

	go func() {
		var err error
		defer func() {
			closeErr := writer.Close()
			if err == nil {
				err = closeErr
			}
			_ = pw.CloseWithError(err)
		}()

		if title != "" {
			if err = writer.WriteField("title", title); err != nil {
				return
			}
		}
		if pageValue != "" {
			if err = writer.WriteField("page", pageValue); err != nil {
				return
			}
		}
		if project != "" {
			if err = writer.WriteField("project_id", project); err != nil {
				return
			}
		}

		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image"; filename="%s"`, escapeQuotes(path.Base(filename))))
		header.Set("Content-Type", contentType)
		var part io.Writer
		part, err = writer.CreatePart(header)
		if err != nil {
			return
		}
		_, err = io.Copy(part, r)
	}()

	// Close the reader when we return so the producer goroutine unblocks (and
	// releases the open upload file) even if doJSON never starts draining it.
	defer pr.Close()
	return c.doJSON(ctx, http.MethodPost, "/api/v1/screenshots", nil, headers, nil, pr)
}

func (c *Client) Annotations(ctx context.Context, screenshot, project string, query url.Values) (json.RawMessage, AnnotationsResponse, error) {
	var out AnnotationsResponse
	if project != "" {
		query = cloneQuery(query)
		query.Set("project_id", project)
	}
	raw, err := c.doJSON(ctx, http.MethodGet, "/api/v1/screenshots/"+url.PathEscape(screenshot)+"/annotations", query, nil, &out, nil)
	return raw, out, err
}

func (c *Client) Annotation(ctx context.Context, id, project string) (json.RawMessage, error) {
	query := url.Values{}
	if project != "" {
		query.Set("project_id", project)
	}
	return c.doJSON(ctx, http.MethodGet, "/api/v1/annotations/"+url.PathEscape(id), query, nil, nil, nil)
}

func (c *Client) AddComment(ctx context.Context, annotation, project, body string) (json.RawMessage, error) {
	form := url.Values{"body": []string{body}}
	if project != "" {
		form.Set("project_id", project)
	}
	headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	return c.doJSON(ctx, http.MethodPost, "/api/v1/annotations/"+url.PathEscape(annotation)+"/comments", nil, headers, nil, strings.NewReader(form.Encode()))
}

func Query(params map[string]string) url.Values {
	values := url.Values{}
	for key, value := range params {
		if value != "" {
			values.Set(key, value)
		}
	}
	return values
}

func WithLimitOffset(values url.Values, limit, offset int) url.Values {
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		values.Set("offset", strconv.Itoa(offset))
	}
	return values
}

func (c *Client) doJSON(ctx context.Context, method, rawPath string, query url.Values, headers map[string]string, out any, body io.Reader) (json.RawMessage, error) {
	u := *c.baseURL
	u.Path = strings.TrimRight(c.baseURL.Path, "/") + rawPath
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if c.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}
	req.Header.Set("Accept", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return raw, parseError(resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return raw, err
		}
	}
	return raw, nil
}

func cloneQuery(values url.Values) url.Values {
	out := url.Values{}
	for key, vals := range values {
		out[key] = append([]string(nil), vals...)
	}
	return out
}

func parseError(status int, raw []byte) error {
	var payload struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &payload)

	message := payload.Error
	if message == "" {
		message = payload.Message
	}
	if message == "" {
		message = http.StatusText(status)
	}
	code := payload.Code
	if code == "" {
		code = statusCode(status)
	}

	return &Error{StatusCode: status, Code: code, Message: message}
}

func statusCode(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "unauthorized"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusTooManyRequests:
		return "rate_limited"
	default:
		return fmt.Sprintf("http_%d", status)
	}
}

func escapeQuotes(value string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, "\\\"").Replace(value)
}
