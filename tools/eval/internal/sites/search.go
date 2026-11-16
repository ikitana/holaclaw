package sites

import (
	"html/template"
	"net/http"
	"regexp"
	"strings"
)

// Task: find the price of a named product. Expected: done, answer has the
// price. Trap: several products share words in their names ("Trail" appears
// three times), so a substring match on the search box alone is not enough —
// the answer must come from the one matching detail page's spec table, not
// from the search results list guessed at.
type product struct {
	Slug, Name, Price, Spec string
}

var searchCatalog = []product{
	{Slug: "trailhead-kettle", Name: "Trailhead Kettle 2L", Price: "$34.50", Spec: "Capacity: 2 litres. Material: stainless steel. Weight: 480 g."},
	{Slug: "trail-runner-socks", Name: "Trail Runner Socks", Price: "$12.00", Spec: "Material: merino wool blend. Sizes: S-XL."},
	{Slug: "trailblazer-tent", Name: "Trailblazer 2-Person Tent", Price: "$189.99", Spec: "Capacity: 2 people. Weight: 2.3 kg. Waterproof rating: 3000 mm."},
	{Slug: "example-lantern", Name: "Example Camp Lantern", Price: "$22.75", Spec: "Battery: 3x AA. Runtime: 40 hours."},
}

var (
	searchTmpl = template.Must(template.New("search").Parse(head("Example Outdoors — Search") + `
<h1>Example Outdoors</h1>
<form method="get" action="{{.Action}}">
  <label for="q">Search products</label>
  <input id="q" name="q" value="{{.Query}}">
  <button type="submit">Search</button>
</form>
{{if .Searched}}
<h2>Results</h2>
{{if .Results}}
<ul>
{{range .Results}}<li><a href="{{.Link}}">{{.Name}}</a> — {{.Price}}</li>
{{end}}</ul>
{{else}}<p>No products matched.</p>{{end}}
{{end}}
` + foot))

	searchItemTmpl = template.Must(template.New("search-item").Parse(head("Example Outdoors — Item") + `
<p><a href="{{.Back}}">&larr; Back to search</a></p>
<h1>{{.Name}}</h1>
<p>Price: {{.Price}}</p>
<table>
<caption>Specifications</caption>
{{range .SpecRows}}<tr><td>{{.}}</td></tr>
{{end}}</table>
` + foot))
)

type searchResult struct{ Name, Price, Link string }

func registerSearch(mux *http.ServeMux) {
	mux.HandleFunc("GET /search/", searchHandler)
	mux.HandleFunc("GET /search/item/{slug}", searchItemHandler)
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	run := runOf(r)
	q := r.URL.Query().Get("q")
	searched := r.URL.Query().Has("q")
	var results []searchResult
	if searched {
		lq := strings.ToLower(strings.TrimSpace(q))
		for _, p := range searchCatalog {
			if lq == "" || strings.Contains(strings.ToLower(p.Name), lq) {
				results = append(results, searchResult{
					Name: p.Name, Price: p.Price,
					Link: withRun("/search/item/"+p.Slug, run),
				})
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = searchTmpl.Execute(w, struct {
		Action   string
		Query    string
		Searched bool
		Results  []searchResult
	}{withRun("/search/", run), q, searched, results})
}

func searchItemHandler(w http.ResponseWriter, r *http.Request) {
	run := runOf(r)
	slug := r.PathValue("slug")
	for _, p := range searchCatalog {
		if p.Slug == slug {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = searchItemTmpl.Execute(w, struct {
				Back, Name, Price string
				SpecRows          []string
			}{withRun("/search/", run), p.Name, p.Price, strings.Split(p.Spec, ". ")})
			return
		}
	}
	http.NotFound(w, r)
}

func searchSite() Site {
	target := searchCatalog[0]
	return Site{
		Name:   "search",
		Path:   "/search/",
		Intent: "Search for \"" + strings.Split(target.Name, " ")[0] + "\" and report the price of \"" + target.Name + "\".",
		Expect: "done, answer has " + target.Price,
		Answer: regexp.MustCompile(regexp.QuoteMeta(target.Price)),
		Check:  func(run string) error { return nil },
	}
}
