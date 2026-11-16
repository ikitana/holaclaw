package sites

import (
	"html/template"
	"net/http"
	"regexp"
	"strconv"
)

// Task: which city on page 3 has the largest population. Expected: done,
// answer names Rivermouth (population 4,210,000, the only city over a
// million anywhere in the table). Trap: rows are not sorted by population —
// scanning only the first page, or the page-1 max, gives a wrong answer, and
// the executor must actually turn to page 3 rather than guess from the
// pager's page-number links alone.
type cityRow struct {
	City       string
	Population string
}

var tableRows = []cityRow{
	{"Eastford", "88,120"}, {"Millhaven", "142,300"}, {"Old Quarry", "51,900"},
	{"Wrenfield", "203,400"}, {"North Barrow", "77,650"}, {"Cedar Hollow", "39,800"},
	{"Fenwick", "165,000"}, {"Ashgrove", "112,450"}, {"Kettlebrook", "68,300"},
	{"Marlow Bend", "94,700"}, // page 1 (1-10)
	{"Stonepath", "121,000"}, {"Harlow Cross", "58,900"}, {"Duskwater", "199,200"},
	{"Pinebridge", "83,400"}, {"Foxhollow", "132,600"}, {"Amberfield", "71,050"},
	{"Cliffend", "145,800"}, {"Greymarsh", "62,300"}, {"Sunder Vale", "108,900"},
	{"Longacre", "97,200"}, // page 2 (11-20)
	{"Bramblewick", "156,700"}, {"Rivermouth", "4,210,000"}, {"Thistledown", "89,300"},
	{"Oakhaven", "133,050"}, {"Windmere", "72,600"}, {"Farrow Point", "101,900"},
	{"Ellsworth", "64,400"}, {"Copperfield", "118,750"}, {"Nettlebrook", "55,200"},
	{"Highstile", "143,300"}, // page 3 (21-30)
	{"Berrow", "48,600"}, {"Crownmoor", "127,400"}, {"Saltmarsh", "91,800"},
	{"Ivywell", "63,900"}, {"Thornfield", "138,200"}, {"Quillwater", "79,500"},
	{"Bracken Hill", "106,300"}, {"Emberdale", "59,700"}, {"Foxglove", "84,100"},
	{"Millrace", "96,600"}, // page 4 (31-40)
}

const tableRowsPerPage = 10
const tableTotalPages = 4

var tableTmpl = template.Must(template.New("table").Parse(head("Example Cities") + `
<h1>Example Cities — page {{.Page}} of {{.TotalPages}}</h1>
<table>
<tr><th>City</th><th>Population</th></tr>
{{range .Rows}}<tr><td>{{.City}}</td><td>{{.Population}}</td></tr>
{{end}}</table>
<nav>
{{if .Prev}}<a href="{{.Prev}}">Previous</a>{{end}}
{{range .PageLinks}}<a href="{{.Link}}">{{.Num}}</a>{{end}}
{{if .Next}}<a href="{{.Next}}">Next</a>{{end}}
</nav>
` + foot))

type tablePageLink struct {
	Num  int
	Link string
}

func registerTable(mux *http.ServeMux) {
	mux.HandleFunc("GET /table/", tableHandler)
}

func tableHandler(w http.ResponseWriter, r *http.Request) {
	run := runOf(r)
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	if page > tableTotalPages {
		page = tableTotalPages
	}
	start := (page - 1) * tableRowsPerPage
	end := start + tableRowsPerPage
	if end > len(tableRows) {
		end = len(tableRows)
	}

	var links []tablePageLink
	for n := 1; n <= tableTotalPages; n++ {
		links = append(links, tablePageLink{Num: n, Link: withRun("/table/?page="+strconv.Itoa(n), run)})
	}
	var prev, next string
	if page > 1 {
		prev = withRun("/table/?page="+strconv.Itoa(page-1), run)
	}
	if page < tableTotalPages {
		next = withRun("/table/?page="+strconv.Itoa(page+1), run)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tableTmpl.Execute(w, struct {
		Page, TotalPages int
		Rows             []cityRow
		PageLinks        []tablePageLink
		Prev, Next       string
	}{page, tableTotalPages, tableRows[start:end], links, prev, next})
}

func tableSite() Site {
	return Site{
		Name:   "table",
		Path:   "/table/",
		Intent: "Go to page 3 of the city table and report which city there has the largest population.",
		Expect: "done, answer names Rivermouth",
		Answer: regexp.MustCompile(`(?i)rivermouth`),
		Check:  func(run string) error { return nil },
	}
}
