package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadNormalizesAndHashesManifest(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "captures", "home.png"), color.RGBA{R: 255, A: 255})
	writePNG(t, filepath.Join(dir, "captures", "home-mobile.png"), color.RGBA{B: 255, A: 255})
	manifestPath := filepath.Join(dir, "snapshot.json")
	writeManifest(t, manifestPath, `{
  "version": 1,
  "git_commit": " ABC1234 ",
  "taken_at": "2026-07-10T12:34:56+01:00",
  "images": [
    {"page":" Home ","file":"captures/home.png","viewport":"DESKTOP"},
    {"page":"Home","title":"Responsive","file":"captures/home-mobile.png","viewport":"mobile"}
  ]
}`)

	prepared, err := load(manifestPath, time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Version != 1 || prepared.GitCommit != "abc1234" || prepared.TakenAt != "2026-07-10T11:34:56.000000Z" {
		t.Fatalf("prepared metadata = %#v", prepared)
	}
	if len(prepared.Images) != 2 || prepared.Images[0].Title != "Home" || prepared.Images[1].Title != "Responsive" {
		t.Fatalf("images = %#v", prepared.Images)
	}
	if prepared.Images[0].MIMEType != "image/png" || prepared.Images[0].ContentSHA256 == "" || prepared.Images[0].FileRefSHA256 == "" {
		t.Fatalf("image identity = %#v", prepared.Images[0])
	}
	if prepared.ManifestDigest == "" || prepared.Images[0].GroupDigest == "" {
		t.Fatalf("missing digests: %#v", prepared)
	}
	if strings.Contains(prepared.ManifestDigest, dir) || strings.Contains(prepared.Images[0].FileRefSHA256, "captures") {
		t.Fatal("digest output leaked readable local paths")
	}
}

func TestCanonicalDigestsMatchRailsContractFixture(t *testing.T) {
	type digestVector struct {
		Namespace  string   `json:"namespace"`
		Components []string `json:"components"`
		SHA256     string   `json:"sha256"`
	}
	var fixture struct {
		Version  int          `json:"version"`
		Manifest digestVector `json:"manifest"`
		Group    digestVector `json:"group"`
	}
	fixturePath := filepath.Join("..", "..", "testdata", "contracts", "snapshot-digests-v1.json")
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != Version {
		t.Fatalf("fixture version = %d want %d", fixture.Version, Version)
	}

	if got, want := digest(fixture.Manifest.Namespace, fixture.Manifest.Components), fixture.Manifest.SHA256; got != want {
		t.Fatalf("manifest digest = %s want %s", got, want)
	}
	if got, want := digest(fixture.Group.Namespace, fixture.Group.Components), fixture.Group.SHA256; got != want {
		t.Fatalf("group digest = %s want %s", got, want)
	}
}

func TestLoadResolvesFilesFromManifestDirectory(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "shot.png"), color.RGBA{G: 255, A: 255})
	manifestPath := filepath.Join(dir, "snapshot.json")
	writeManifest(t, manifestPath, validManifestJSON("shot.png"))

	other := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	prepared, err := load(manifestPath, time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Images[0].rootPath != dir || prepared.Images[0].fileRef != "shot.png" {
		t.Fatalf("root=%q ref=%q", prepared.Images[0].rootPath, prepared.Images[0].fileRef)
	}
}

func TestLoadRejectsSymlinkOutsideManifestDirectory(t *testing.T) {
	parent := t.TempDir()
	manifestDir := filepath.Join(parent, "manifest")
	if err := os.Mkdir(manifestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.png")
	writePNG(t, outside, color.RGBA{R: 44, A: 255})
	if err := os.Symlink(outside, filepath.Join(manifestDir, "escape.png")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	manifestPath := filepath.Join(manifestDir, "snapshot.json")
	writeManifest(t, manifestPath, validManifestJSON("escape.png"))

	_, err := load(manifestPath, time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC))
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Code != "invalid_file" {
		t.Fatalf("error = %#v", err)
	}
	if strings.Contains(err.Error(), parent) || strings.Contains(err.Error(), "outside.png") {
		t.Fatalf("error leaked local path: %v", err)
	}
}

func TestLoadAcceptsSymlinkWhoseTargetStaysInsideManifestDirectory(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "target.png"), color.RGBA{G: 44, A: 255})
	if err := os.Symlink("target.png", filepath.Join(dir, "capture.png")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	manifestPath := filepath.Join(dir, "snapshot.json")
	writeManifest(t, manifestPath, validManifestJSON("capture.png"))

	if _, err := load(manifestPath, time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAcceptsJPEGBytesIndependentlyOfExtension(t *testing.T) {
	dir := t.TempDir()
	writeJPEG(t, filepath.Join(dir, "capture.bin"), color.RGBA{R: 12, G: 34, B: 56, A: 255})
	manifestPath := filepath.Join(dir, "snapshot.json")
	writeManifest(t, manifestPath, validManifestJSON("capture.bin"))

	prepared, err := load(manifestPath, time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Images[0].MIMEType != "image/jpeg" {
		t.Fatalf("mime type = %q", prepared.Images[0].MIMEType)
	}
}

func TestCheckedInValidManifestFixturePreflights(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "manifests", "valid.json")
	prepared, err := load(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Images) != 2 || prepared.ManifestDigest == "" {
		t.Fatalf("prepared fixture = %#v", prepared)
	}
	if prepared.Images[0].Title != "Homepage" || prepared.Images[1].Title != "Homepage" {
		t.Fatalf("viewport variants must use one logical title: %#v", prepared.Images)
	}
	if prepared.Images[0].GroupDigest != prepared.Images[1].GroupDigest {
		t.Fatalf("viewport variants were split into different screenshot groups: %#v", prepared.Images)
	}
}

func TestLoadRejectsCorrelatedViewportSuffixesInSeparateTitles(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "shot.png"), color.RGBA{A: 255})
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	titlePairs := [][2]string{
		{"Benchmark overview — desktop", "Benchmark overview — mobile"},
		{"Homepage desktop", "Homepage mobile"},
		{"Homepage.desktop", "Homepage.mobile"},
		{"Checkout (desktop)", "Checkout (mobile)"},
		{"desktop", "mobile"},
		{"Hero: desktop viewport", "Hero: mobile viewport"},
		{"Account settings", "Account settings / mobile"},
	}
	for _, titles := range titlePairs {
		t.Run(titles[0]+" and "+titles[1], func(t *testing.T) {
			manifest := Manifest{
				Version:   1,
				GitCommit: "abc1234",
				TakenAt:   "2026-07-10T10:00:00Z",
				Images: []ImageEntry{
					{Page: "Home", Title: titles[0], File: "shot.png", Viewport: "desktop"},
					{Page: "Home", Title: titles[1], File: "shot.png", Viewport: "mobile"},
				},
			}
			_, err := prepareManifest(dir, manifest, now)
			var validation *ValidationError
			if !errors.As(err, &validation) || validation.Code != "viewport_in_title" || validation.Index == nil || *validation.Index != 1 {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestLoadAllowsLoneOrSharedLogicalTitleEndingInViewportWord(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "shot.png"), color.RGBA{A: 255})
	manifests := []Manifest{
		{
			Version:   1,
			GitCommit: "abc1234",
			TakenAt:   "2026-07-10T10:00:00Z",
			Images: []ImageEntry{{
				Page: "Docs", Title: "Platform / Mobile", File: "shot.png", Viewport: "mobile",
			}},
		},
		{
			Version:   1,
			GitCommit: "abc1234",
			TakenAt:   "2026-07-10T10:00:00Z",
			Images: []ImageEntry{
				{Page: "Docs", Title: "Platform / Mobile", File: "shot.png", Viewport: "desktop"},
				{Page: "Docs", Title: "Platform / Mobile", File: "shot.png", Viewport: "mobile"},
			},
		},
		{
			Version:   1,
			GitCommit: "abc1234",
			TakenAt:   "2026-07-10T10:00:00Z",
			Images: []ImageEntry{{
				Page: "Docs", Title: "Mobile account settings", File: "shot.png", Viewport: "mobile",
			}},
		},
		{
			Version:   1,
			GitCommit: "abc1234",
			TakenAt:   "2026-07-10T10:00:00Z",
			Images: []ImageEntry{
				{Page: "Docs", Title: "Platform", File: "shot.png", Viewport: "mobile"},
				{Page: "Docs", Title: "Platform / Mobile", File: "shot.png", Viewport: "mobile"},
			},
		},
	}

	for _, manifest := range manifests {
		if _, err := prepareManifest(dir, manifest, time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestManifestIdentityChangesForEverySemanticInput(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "one.png"), color.RGBA{R: 1, A: 255})
	writePNG(t, filepath.Join(dir, "two.png"), color.RGBA{R: 2, A: 255})
	oneBytes, err := os.ReadFile(filepath.Join(dir, "one.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alias.png"), oneBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	base := Manifest{
		Version:   1,
		GitCommit: "abc1234",
		TakenAt:   "2026-07-10T10:00:00Z",
		Images: []ImageEntry{
			{Page: "Home", File: "one.png", Viewport: "desktop"},
			{Page: "Home", File: "two.png", Viewport: "mobile"},
		},
	}
	baseDigest := prepare(t, dir, base, now).ManifestDigest
	mutations := []Manifest{
		withCommit(base, "def5678"),
		withTakenAt(base, "2026-07-10T10:00:01Z"),
		withImageMutation(base, 0, func(entry *ImageEntry) { entry.Page = "Checkout" }),
		withImageMutation(base, 0, func(entry *ImageEntry) { entry.Title = "Hero" }),
		withImageMutation(base, 0, func(entry *ImageEntry) { entry.Viewport = "tablet" }),
		withImageMutation(base, 0, func(entry *ImageEntry) { entry.File = "alias.png" }),
		withImageMutation(base, 0, func(entry *ImageEntry) { entry.File = "two.png"; entry.Viewport = "tablet" }),
		withReversedImages(base),
	}
	for i, manifest := range mutations {
		if got := prepare(t, dir, manifest, now).ManifestDigest; got == baseDigest {
			t.Fatalf("mutation %d did not change digest", i)
		}
	}

	writePNG(t, filepath.Join(dir, "one.png"), color.RGBA{B: 4, A: 255})
	if got := prepare(t, dir, base, now).ManifestDigest; got == baseDigest {
		t.Fatal("changed bytes did not change digest")
	}
}

func TestLoadRejectsInvalidInputsWithoutLeakingPaths(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "shot.png"), color.RGBA{A: 255})
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"unknown version": strings.Replace(validManifestJSON("shot.png"), `"version":1`, `"version":2`, 1),
		"bad commit":      strings.Replace(validManifestJSON("shot.png"), `"abc1234"`, `"bad"`, 1),
		"bad timestamp":   strings.Replace(validManifestJSON("shot.png"), `"2026-07-10T10:00:00Z"`, `"2026-07-10"`, 1),
		"missing file":    validManifestJSON("missing.png"),
		"escaping path":   validManifestJSON("../shot.png"),
		"bad viewport":    strings.Replace(validManifestJSON("shot.png"), `"desktop"`, `"watch"`, 1),
		"duplicate": `{"version":1,"git_commit":"abc1234","taken_at":"2026-07-10T10:00:00Z","images":[
          {"page":"Home","file":"shot.png","viewport":"desktop"},
          {"page":"Home","file":"shot.png","viewport":"desktop"}]}`,
		"empty": `{"version":1,"git_commit":"abc1234","taken_at":"2026-07-10T10:00:00Z","images":[]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
			writeManifest(t, path, raw)
			_, err := load(path, now)
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "missing.png") {
				t.Fatalf("error leaked a local path: %v", err)
			}
		})
	}

	invalidImage := filepath.Join(dir, "invalid.png")
	if err := os.WriteFile(invalidImage, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "invalid-image.json")
	writeManifest(t, manifestPath, validManifestJSON("invalid.png"))
	if _, err := load(manifestPath, now); err == nil || !strings.Contains(err.Error(), "PNG or JPEG") {
		t.Fatalf("invalid image error = %v", err)
	}

	oversized := filepath.Join(dir, "oversized.png")
	file, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxFileSize + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	manifestPath = filepath.Join(dir, "oversized-image.json")
	writeManifest(t, manifestPath, validManifestJSON("oversized.png"))
	if _, err := load(manifestPath, now); err == nil || !strings.Contains(err.Error(), "20MB") {
		t.Fatalf("oversized image error = %v", err)
	}
}

func TestOpenVerifiedDetectsChangedBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	writePNG(t, path, color.RGBA{R: 8, A: 255})
	manifestPath := filepath.Join(dir, "snapshot.json")
	writeManifest(t, manifestPath, validManifestJSON("shot.png"))
	prepared, err := load(manifestPath, time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	writePNG(t, path, color.RGBA{B: 9, A: 255})

	file, err := prepared.Images[0].OpenVerified()
	if file != nil {
		_ = file.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "changed after preflight") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenVerifiedReturnsAnImmutableTemporaryCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	writePNG(t, path, color.RGBA{R: 8, A: 255})
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "snapshot.json")
	writeManifest(t, manifestPath, validManifestJSON("shot.png"))
	prepared, err := load(manifestPath, time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	upload, err := prepared.Images[0].OpenVerified()
	if err != nil {
		t.Fatal(err)
	}
	writePNG(t, path, color.RGBA{B: 9, A: 255})
	got, err := io.ReadAll(upload)
	if err != nil {
		t.Fatal(err)
	}
	temporaryPath := upload.(*verifiedUpload).path
	if err := upload.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("upload bytes changed after the original path was replaced")
	}
	if _, err := os.Stat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary upload still exists: %v", err)
	}
}

func prepare(t *testing.T, dir string, manifest Manifest, now time.Time) *PreparedManifest {
	t.Helper()
	path := filepath.Join(dir, "identity.json")
	raw := marshalManifest(t, manifest)
	writeManifest(t, path, string(raw))
	prepared, err := load(path, now)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func writePNG(t *testing.T, path string, c color.Color) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.Set(x, y, c)
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJPEG(t *testing.T, path string, c color.Color) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.Set(x, y, c)
		}
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := jpeg.Encode(file, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
}

func writeManifest(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func validManifestJSON(file string) string {
	return `{"version":1,"git_commit":"abc1234","taken_at":"2026-07-10T10:00:00Z","images":[{"page":"Home","file":"` + file + `","viewport":"desktop"}]}`
}

func marshalManifest(t *testing.T, manifest Manifest) []byte {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func cloneManifest(manifest Manifest) Manifest {
	cloned := manifest
	cloned.Images = append([]ImageEntry(nil), manifest.Images...)
	return cloned
}

func withCommit(manifest Manifest, value string) Manifest {
	cloned := cloneManifest(manifest)
	cloned.GitCommit = value
	return cloned
}

func withTakenAt(manifest Manifest, value string) Manifest {
	cloned := cloneManifest(manifest)
	cloned.TakenAt = value
	return cloned
}

func withImageMutation(manifest Manifest, index int, mutate func(*ImageEntry)) Manifest {
	cloned := cloneManifest(manifest)
	mutate(&cloned.Images[index])
	return cloned
}

func withReversedImages(manifest Manifest) Manifest {
	cloned := cloneManifest(manifest)
	cloned.Images[0], cloned.Images[1] = cloned.Images[1], cloned.Images[0]
	return cloned
}
