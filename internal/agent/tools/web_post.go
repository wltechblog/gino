package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WebPostTool performs HTTP POST/PUT/PATCH requests with an inline body or
// with multipart file uploads streamed directly from disk.
//
// Its purpose is to keep binary payloads OUT of the LLM message chain: a
// file upload passes only the path; the bytes travel from the filesystem
// tool's sandboxed roots to the socket, never through a tool call result
// or a message.
//
// Args:
//
//	{
//	  "url":     "https://api.example.com/v1/upload",
//	  "method":  "POST",                    // POST | PUT | PATCH (default POST)
//	  "headers": {"Authorization": "..."},  // optional custom headers
//	  "body":    "raw string body",         // inline body (sent as-is)
//	  "json":    {"any": "object"},         // marshaled + application/json
//	  "files":   [{"path": "img.png", "field": "file", "filename": "img.png"}],
//	  "fields":  {"extra": "form field"},   // extra multipart form fields (with files)
//	}

const (
	// inlinePostBodyHardCap caps body/json arguments (LLM-authored text).
	// Files stream from disk and are not bounded by this.
	inlinePostBodyHardCap = 64 << 20
	maxPostFiles          = 16
	// minUploadTimeoutS floors the request deadline when files are streamed,
	// so large transfers aren't cut short by the default web timeout.
	minUploadTimeoutS = 600
)

// WebPostTool shares its configuration surface with the web tool (timeout,
// max response bytes, user agent) so one config block governs both.
type WebPostTool struct {
	timeout   time.Duration
	maxBytes  int64
	userAgent string
	client    *http.Client
	fs        *FilesystemTool
}

// NewWebPostTool creates a POST-capable tool. fs may be nil, in which case
// the files parameter is rejected (the gateway always wires the real one).
// Request deadlines are enforced via context, so the shared client sets no
// Timeout of its own.
func NewWebPostTool(timeoutS, maxBytes int, userAgent string, fs *FilesystemTool) *WebPostTool {
	if timeoutS <= 0 {
		timeoutS = defaultWebTimeoutS
	}
	if maxBytes <= 0 {
		maxBytes = defaultWebMaxBytes
	}
	if maxBytes > maxAllowedResponseBytes {
		maxBytes = maxAllowedResponseBytes
	}
	if userAgent == "" {
		userAgent = defaultWebUserAgent
	}
	return &WebPostTool{
		timeout:   time.Duration(timeoutS) * time.Second,
		maxBytes:  int64(maxBytes),
		userAgent: userAgent,
		client:    &http.Client{},
		fs:        fs,
	}
}

func (t *WebPostTool) Name() string { return "web_post" }
func (t *WebPostTool) Description() string {
	return "HTTP POST/PUT/PATCH with inline body or multipart file upload streamed from disk (file contents never enter the conversation)"
}

func (t *WebPostTool) Parameters() map[string]interface{} {
	fileItemProps := map[string]interface{}{
		"path": map[string]interface{}{
			"type":        "string",
			"description": "Path to the file on disk (sandboxed like the filesystem tool)",
		},
		"field": map[string]interface{}{
			"type":        "string",
			"description": "Form field name for the file part (default 'file')",
		},
		"filename": map[string]interface{}{
			"type":        "string",
			"description": "Filename reported to the server (default: base of path)",
		},
	}
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"url": map[string]interface{}{
				"type":        "string",
				"description": "The target URL (must be http or https)",
			},
			"method": map[string]interface{}{
				"type":        "string",
				"description": "HTTP method: POST, PUT or PATCH (default POST)",
			},
			"headers": map[string]interface{}{
				"type":        "object",
				"description": "Optional request headers as a string map (e.g. Authorization)",
			},
			"body": map[string]interface{}{
				"type":        "string",
				"description": "Inline request body as a string (sent as-is)",
			},
			"json": map[string]interface{}{
				"type":        "object",
				"description": "JSON request body (marshaled and sent with application/json content type)",
			},
			"files": map[string]interface{}{
				"type":        "array",
				"items":       map[string]interface{}{"type": "object", "properties": fileItemProps},
				"description": "Files to upload as multipart/form-data, streamed from disk",
			},
			"fields": map[string]interface{}{
				"type":        "object",
				"description": "Extra form fields sent alongside files (multipart mode)",
			},
		},
		"required": []string{"url"},
	}
}

// Execute performs the request. Only text responses are returned to the LLM
// (same policy as the web tool); binary responses are rejected with a hint.
func (t *WebPostTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	u, _ := args["url"].(string)
	if u == "" {
		return "", fmt.Errorf("web_post: 'url' argument required")
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "", fmt.Errorf("web_post: only http and https URLs are supported")
	}
	u = strings.ReplaceAll(u, `\u0026`, "&")
	u = strings.ReplaceAll(u, `\u003d`, "=")

	method := strings.ToUpper(stringArg(args, "method", "POST"))
	switch method {
	case "POST", "PUT", "PATCH":
	default:
		return "", fmt.Errorf("web_post: method must be POST, PUT or PATCH (got %q)", method)
	}

	files := fileSpecs(args["files"])
	fields, _ := args["fields"].(map[string]interface{})
	bodyStr, hasBody := args["body"].(string)
	jsonVal, hasJSON := args["json"]

	if len(files) > 0 && (hasBody || hasJSON) {
		return "", fmt.Errorf("web_post: 'files' cannot be combined with 'body' or 'json' (multipart vs inline)")
	}
	if hasBody && hasJSON {
		return "", fmt.Errorf("web_post: use either 'body' or 'json', not both")
	}
	if fields != nil && len(files) == 0 {
		return "", fmt.Errorf("web_post: 'fields' only applies to multipart uploads — provide 'files' too")
	}
	if len(files) > maxPostFiles {
		return "", fmt.Errorf("web_post: too many files (%d, max %d)", len(files), maxPostFiles)
	}

	// Deadline: uploads get a floor so big streams aren't cut by the
	// default web timeout. Context governs; client has no Timeout.
	deadline := t.timeout
	if len(files) > 0 && deadline < minUploadTimeoutS*time.Second {
		deadline = minUploadTimeoutS * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	// ---- body ------------------------------------------------------------
	var reader io.Reader
	var contentType string

	if len(files) > 0 {
		if t.fs == nil {
			return "", fmt.Errorf("web_post: file uploads unavailable (filesystem tool not wired)")
		}
		// Pre-open every file through the sandboxed roots BEFORE dialing,
		// so a rejected path (or missing file) fails the tool call outright
		// instead of producing a half-sent request. Handles close after the
		// transfer completes (or fails).
		srcs := make([]*os.File, len(files))
		for i, f := range files {
			src, err := t.fs.openSandboxed(f.path)
			if err != nil {
				for _, s := range srcs[:i] {
					_ = s.Close()
				}
				return "", fmt.Errorf("web_post: open %q: %w", f.path, err)
			}
			srcs[i] = src
		}

		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		contentType = mw.FormDataContentType()

		go func() {
			err := writeMultipartBody(mw, fields, files, srcs)
			for _, s := range srcs {
				_ = s.Close()
			}
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			_ = pw.Close()
		}()

		reader = pr
	} else if hasJSON {
		buf, err := json.Marshal(jsonVal)
		if err != nil {
			return "", fmt.Errorf("web_post: marshal json body: %w", err)
		}
		if len(buf) > inlinePostBodyHardCap {
			return "", fmt.Errorf("web_post: body exceeds %d byte hard cap", inlinePostBodyHardCap)
		}
		reader = bytes.NewReader(buf)
		contentType = "application/json"
	} else if hasBody {
		if len(bodyStr) > inlinePostBodyHardCap {
			return "", fmt.Errorf("web_post: body exceeds %d byte hard cap", inlinePostBodyHardCap)
		}
		reader = strings.NewReader(bodyStr)
	} // else: empty body

	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", t.userAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if err := applyHeaders(req, args["headers"]); err != nil {
		return "", err
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if ct != "" && !isTextContentType(ct) {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("web_post: unsupported response content type %q — only text formats are returned; save binary responses via curl instead", ct)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, t.maxBytes+1))
	if err != nil {
		return "", err
	}
	statusLine := fmt.Sprintf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	if int64(len(respBody)) > t.maxBytes {
		return statusLine + "\n" + string(respBody[:t.maxBytes]) + fmt.Sprintf("\n\n[response truncated at %d bytes]", t.maxBytes), nil
	}
	return statusLine + "\n" + string(respBody), nil
}

// writeMultipartBody writes all form fields then file parts. Files are
// pre-opened by the caller (sandbox-validated) and closed there too.
// Streaming through the pipe means a file of any size never occupies RAM.
func writeMultipartBody(mw *multipart.Writer, fields map[string]interface{}, files []postFileSpec, srcs []*os.File) error {
	for k, v := range fields {
		s, ok := v.(string)
		if !ok {
			b, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("web_post: field %q: %w", k, err)
			}
			s = string(b)
		}
		if err := mw.WriteField(k, s); err != nil {
			return err
		}
	}
	for i, f := range files {
		field := f.field
		if field == "" {
			field = "file"
		}
		filename := f.filename
		if filename == "" {
			filename = filepath.Base(f.path)
		}
		hdr := make(textproto.MIMEHeader)
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
			multipartEscapeQuotes(field), multipartEscapeQuotes(filename)))
		hdr.Set("Content-Type", detectContentTypeFor(filename))
		part, err := mw.CreatePart(hdr)
		if err != nil {
			return err
		}
		if _, err := io.Copy(part, srcs[i]); err != nil {
			return fmt.Errorf("web_post: stream %q: %w", f.path, err)
		}
	}
	return mw.Close()
}

// postFileSpec is one entry of the files argument.
type postFileSpec struct {
	path     string
	field    string
	filename string
}

// fileSpecs normalizes the files argument (list of maps); entries without a
// path are dropped.
func fileSpecs(v interface{}) []postFileSpec {
	raw, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]postFileSpec, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]interface{})
		if m == nil {
			continue
		}
		p, _ := m["path"].(string)
		if p == "" {
			continue
		}
		field, _ := m["field"].(string)
		filename, _ := m["filename"].(string)
		out = append(out, postFileSpec{path: p, field: field, filename: filename})
	}
	return out
}

// applyHeaders copies a string map of custom headers onto the request.
func applyHeaders(req *http.Request, v interface{}) error {
	hdrs, ok := v.(map[string]interface{})
	if !ok || hdrs == nil {
		return nil
	}
	for k, val := range hdrs {
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("web_post: header %q must be a string", k)
		}
		req.Header.Set(k, s)
	}
	return nil
}

// stringArg fetches a string arg with a default.
func stringArg(args map[string]interface{}, key, def string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return def
}

// multipartEscapeQuotes sanitizes a form name/filename for the
// Content-Disposition header (same escaping mime/multipart applies).
func multipartEscapeQuotes(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// detectContentTypeFor guesses a MIME type from the filename extension,
// falling back to application/octet-stream (matching http.DetectContentType
// for the types agents actually upload).
func detectContentTypeFor(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".txt", ".md", ".csv":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".zip":
		return "application/zip"
	case ".mp3":
		return "audio/mpeg"
	case ".mp4":
		return "video/mp4"
	case ".wav":
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}
