package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	port        = "13387"
	maxUploadMB = 8
)

var (
	nameAllowRe = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/upload", handleUpload)
	mux.HandleFunc("/render", handleRender)

	log.Printf("Listening on :%s", port)
	if err := http.ListenAndServe("0.0.0.0:"+port, logRequests(securityHeaders(mux))); err != nil {
		log.Fatal(err)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s", r.RemoteAddr, r.Method, r.URL)
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "0")
		next.ServeHTTP(w, r)
	})
}

func userID(r *http.Request) string {
	uid := r.Header.Get("X-User")
	if uid == "" {
		uid = "anon"
	}
	// restrict characters
	clean := make([]rune, 0, len(uid))
	for _, ch := range uid {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' {
			clean = append(clean, ch)
		}
	}
	if len(clean) == 0 {
		return "anon"
	}
	return string(clean)
}

func workspace(r *http.Request) string {
	return filepath.Join("data", userID(r))
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// ensure workspace exists
	ws := workspace(r)
	_ = os.MkdirAll(ws, 0o755)

	var names []string
	_ = filepath.WalkDir(ws, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".tmpl") {
			rel, _ := filepath.Rel(ws, p)
			rel = strings.TrimSuffix(rel, ".tmpl")
			rel = strings.ReplaceAll(rel, string(filepath.Separator), "/")
			names = append(names, rel)
		}
		return nil
	})

	t, err := baseTemplate()
	if err != nil {
		http.Error(w, "template error", 500)
		return
	}
	data := map[string]any{
		"Templates": names,
	}
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "render error", 500)
	}
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(maxUploadMB << 20); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	file, hdr, err := r.FormFile("bundle")
	if err != nil {
		http.Error(w, "missing bundle", 400)
		return
	}
	defer file.Close()

	if !strings.HasSuffix(strings.ToLower(hdr.Filename), ".zip") {
		http.Error(w, "bundle must be .zip", 400)
		return
	}

	buf := new(bytes.Buffer)
	if _, err := io.CopyN(buf, file, maxUploadMB<<20); err != nil && err != io.EOF {
		http.Error(w, "bundle too large", 413)
		return
	}

	ws := workspace(r)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		http.Error(w, "server error", 500)
		return
	}

	if err := extractZip(buf.Bytes(), ws); err != nil {
		http.Error(w, "invalid zip", 400)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func extractZip(data []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		// normalize name
		name := strings.ReplaceAll(f.Name, "\\", "/")
		name = path.Clean("/" + name)[1:] 
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			continue
		}

		target := filepath.Join(dest, filepath.FromSlash(name))
		if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), filepath.Clean(dest)+string(os.PathSeparator)) {
			continue
		}

		mode := f.Mode()
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}

		// ensure parent dir
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}
		content, _ := io.ReadAll(rc)
		rc.Close()

		if mode&os.ModeSymlink != 0 {
			linkTarget := string(bytes.TrimSpace(content))
			if err := os.Symlink(linkTarget, target); err != nil {
				return err
			}
			continue
		}

		if err := os.WriteFile(target, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func handleRender(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" || !nameAllowRe.MatchString(name) || strings.Contains(name, "..") {
		http.Error(w, "bad name", 400)
		return
	}

	ws := workspace(r)
	// read user template
	userTmplPath, err := secureJoin(ws, name+".tmpl")
	if err != nil {
		http.Error(w, "bad path", 400)
		return
	}
	if _, err := os.Stat(userTmplPath); err != nil {
		http.Error(w, "not found", 404)
		return
	}

	t, err := baseTemplateWithWS(ws)
	if err != nil {
		http.Error(w, "template error", 500)
		return
	}

	if _, err := t.ParseFiles(userTmplPath); err != nil {
		http.Error(w, "parse error", 400)
		return
	}

	data := map[string]any{
		"User": userID(r),
		"Now":  time.Now().Format(time.RFC3339),
	}
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "render error", 500)
		return
	}
}

func baseTemplate() (*template.Template, error) {
	funcs := template.FuncMap{
		"hash": func(s string) string {
			sum := sha256.Sum256([]byte(s))
			return hex.EncodeToString(sum[:])
		},
		"safe": func(s string) template.HTML {
			return template.HTML(s)
		},
	}
	t := template.New("layout").Funcs(funcs)
	return t.ParseFiles(
		filepath.Join("templates", "layout.tmpl"),
		filepath.Join("templates", "index.tmpl"),
	)
}

func baseTemplateWithWS(ws string) (*template.Template, error) {
	funcs := template.FuncMap{
		"hash": func(s string) string {
			sum := sha256.Sum256([]byte(s))
			return hex.EncodeToString(sum[:])
		},
		"safe": func(s string) template.HTML { return template.HTML(s) },
		"read": func(rel string) string {
			if !nameAllowRe.MatchString(rel) || strings.Contains(rel, "..") {
				return "[deny]"
			}
			low := strings.ToLower(rel)
			if strings.Contains(low, "flag") {
				return "[deny]"
			}
			allowed := []string{".txt", ".md"}
			if !slices.Contains(allowed, strings.ToLower(filepath.Ext(rel))) {
				return "[deny]"
			}
			joined, err := secureJoin(ws, rel) 
			if err != nil {
				return "[deny]"
			}
			b, err := os.ReadFile(joined) 
			if err != nil {
				return "[err]"
			}
			return string(b)
		},
	}
	t := template.New("layout").Funcs(funcs)
	return t.ParseFiles(
		filepath.Join("templates", "layout.tmpl"),
		filepath.Join("templates", "index.tmpl"),
	)
}

func secureJoin(base, rel string) (string, error) {
	cleanRel := filepath.Clean(rel)
	if strings.HasPrefix(cleanRel, "..") || strings.Contains(cleanRel, "\\") {
		return "", os.ErrPermission
	}
	full := filepath.Join(base, cleanRel)
	if !strings.HasPrefix(filepath.Clean(full)+string(os.PathSeparator), filepath.Clean(base)+string(os.PathSeparator)) {
		return "", os.ErrPermission
	}
	return full, nil
}
