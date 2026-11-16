package sites

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
)

// Task: download the report. Expected: one download named report.csv. Trap:
// a second link streams 60 MiB, over the sidecar's per-file cap
// (proto.MaxDownloadBytes = 50 MiB) — a plan that clicks "Download huge"
// instead of "Download report" must be refused, not silently truncated.
const (
	downloadReportBytes = 20 << 10 // 20 KiB
	downloadHugeBytes   = 60 << 20 // 60 MiB, over proto.MaxDownloadBytes
)

var downloadPageTmpl = template.Must(template.New("download").Parse(head("Example Reports") + `
<h1>Example Reports</h1>
<p><a href="{{.Report}}" download="report.csv">Download report</a></p>
<p><a href="{{.Huge}}" download="huge.csv">Download huge</a></p>
` + foot))

func registerDownload(mux *http.ServeMux) {
	mux.HandleFunc("GET /download/", func(w http.ResponseWriter, r *http.Request) {
		run := runOf(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = downloadPageTmpl.Execute(w, struct{ Report, Huge string }{
			withRun("/download/report.csv", run), withRun("/download/huge", run),
		})
	})
	mux.HandleFunc("GET /download/report.csv", downloadReportHandler)
	mux.HandleFunc("GET /download/huge", downloadHugeHandler)
}

func downloadReportHandler(w http.ResponseWriter, r *http.Request) {
	body := buildCSV(downloadReportBytes)
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="report.csv"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// buildCSV returns invented row,population-style CSV data at least
// minBytes long.
func buildCSV(minBytes int) []byte {
	out := []byte("id,label,value\n")
	for i := 1; len(out) < minBytes; i++ {
		out = append(out, fmt.Sprintf("%d,example-row-%d,%d\n", i, i, i*7)...)
	}
	return out
}

func downloadHugeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="huge.csv"`)
	w.Header().Set("Content-Length", strconv.Itoa(downloadHugeBytes))
	flusher, _ := w.(http.Flusher)
	const chunkSize = 1 << 20 // 1 MiB, never the whole body at once
	chunk := make([]byte, chunkSize)
	for i := range chunk {
		chunk[i] = 'x'
	}
	written := 0
	for written < downloadHugeBytes {
		n := chunkSize
		if remaining := downloadHugeBytes - written; remaining < n {
			n = remaining
		}
		if _, err := w.Write(chunk[:n]); err != nil {
			return
		}
		written += n
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func downloadSite() Site {
	return Site{
		Name:   "download",
		Path:   "/download/",
		Intent: "Download the report.",
		Expect: "done, one download named report.csv; \"Download huge\" is refused by the per-file cap",
		Check:  func(run string) error { return nil },
	}
}
