package screenote

type Project struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	Role            string `json:"role,omitempty"`
	ScreenshotCount int    `json:"screenshot_count"`
	CreatedAt       string `json:"created_at"`
}

type ProjectsResponse struct {
	Projects []Project `json:"projects"`
}

type Page struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	VersionCount int    `json:"version_count"`
	URL          string `json:"url"`
	CreatedAt    string `json:"created_at"`
}

type Pagination struct {
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

type ScreenshotImage struct {
	ID       int    `json:"id"`
	Viewport string `json:"viewport"`
	Status   string `json:"status"`
	Width    *int   `json:"width"`
	Height   *int   `json:"height"`
	Attached bool   `json:"attached"`
}

type Screenshot struct {
	ID              int               `json:"id"`
	Title           string            `json:"title"`
	PageID          int               `json:"page_id"`
	PageName        string            `json:"page_name"`
	Status          string            `json:"status"`
	AnnotationCount int               `json:"annotation_count"`
	UnresolvedCount int               `json:"unresolved_count"`
	AnnotateURL     string            `json:"annotate_url"`
	Viewports       []ScreenshotImage `json:"viewports"`
	CreatedAt       string            `json:"created_at"`
}

type ScreenshotsResponse struct {
	Screenshots []Screenshot `json:"screenshots"`
	Pagination  Pagination   `json:"pagination"`
}

type Coordinates struct {
	XPercent      float64  `json:"x_percent"`
	YPercent      float64  `json:"y_percent"`
	WidthPercent  *float64 `json:"width_percent"`
	HeightPercent *float64 `json:"height_percent"`
}

type Annotation struct {
	ID                 int         `json:"id"`
	ScreenshotID       int         `json:"screenshot_id"`
	Viewport           string      `json:"viewport"`
	Type               string      `json:"type"`
	Coordinates        Coordinates `json:"coordinates"`
	Comment            string      `json:"comment"`
	Status             string      `json:"status"`
	Author             string      `json:"author"`
	CommentsCount      int         `json:"comments_count"`
	CreatedAt          string      `json:"created_at"`
	ScreenshotStatus   string      `json:"screenshot_status,omitempty"`
	CroppedImageBase64 *string     `json:"cropped_image_base64,omitempty"`
	MIMEType           string      `json:"mime_type,omitempty"`
	Comments           []Comment   `json:"comments,omitempty"`
}

type AnnotationsResponse struct {
	Annotations []Annotation `json:"annotations"`
	Pagination  Pagination   `json:"pagination"`
}

type Comment struct {
	ID           int    `json:"id"`
	AnnotationID int    `json:"annotation_id,omitempty"`
	Action       string `json:"action"`
	Body         string `json:"body"`
	Author       string `json:"author"`
	CreatedAt    string `json:"created_at"`
}
