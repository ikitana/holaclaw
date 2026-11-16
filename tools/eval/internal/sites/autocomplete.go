package sites

import (
	"html/template"
	"net/http"
	"regexp"
)

// Task: find the cheapest one-way train from Madrid to Barcelona on 15
// October 2026 and report its price. Expected: done, answer has 32,40.
// Traps, in the order the agent hits them:
//   - a cookie modal (role="dialog" aria-modal="true") sits over the page on
//     first load, with a transparent overlay under it, so nothing on the
//     form is clickable until "Aceptar" or "Rechazar" is clicked;
//   - both station fields are autocompletes (role="combobox" inputs with an
//     empty role="listbox" filled by inline JS) — typing "Madrid" and
//     pressing Search sends the field empty, because the hidden id the
//     server actually reads is only set by clicking a suggestion; any
//     further typing after a click clears that hidden id again, so a raw
//     typed value never counts as a chosen station;
//   - the trip-type control is a div with role="combobox", not an input, so
//     it cannot be typed into at all — it must be clicked open and an
//     option ("Solo ida") clicked to switch off the "Ida y vuelta" default;
//   - the date field is a readonly input: typing into it does nothing, and
//     a date is only accepted by opening its dialog and clicking a day
//     button, which is the only thing that fills the hidden date the server
//     reads.
//
// A one-way trip also drops the "Vuelta" table, so the agent's cheapest-price
// answer has exactly one right value instead of two.
type acStation struct{ Code, Name string }

var acStations = []acStation{
	{"MAD-ATO", "Madrid-Puerta de Atocha"},
	{"MAD-CHM", "Madrid-Chamartín"},
	{"BCN-SAN", "Barcelona-Sants"},
	{"VLC-JSO", "Valencia-Joaquín Sorolla"},
	{"SEV-SJU", "Sevilla-Santa Justa"},
	{"MAL-MZA", "Málaga-María Zambrano"},
}

func acStationName(code string) (string, bool) {
	for _, s := range acStations {
		if s.Code == code {
			return s.Name, true
		}
	}
	return "", false
}

type acTrain struct {
	Time     string
	Price    string
	Cheapest bool
}

// acTrains (outbound, "Ida") and acReturnTrains (inbound, "Vuelta") are both
// deterministic so the eval's expected answer never moves. Their cheapest
// fares differ (32,40 vs 29,90) so a one-way question has exactly one right
// answer even though a round-trip search shows both tables.
var acTrains = []acTrain{
	{Time: "07:15", Price: "42,00 €"},
	{Time: "09:30", Price: "32,40 €", Cheapest: true},
	{Time: "13:50", Price: "55,75 €"},
}

var acReturnTrains = []acTrain{
	{Time: "16:05", Price: "38,00 €"},
	{Time: "18:20", Price: "29,90 €", Cheapest: true},
	{Time: "21:45", Price: "46,10 €"},
}

// acOctoberDays is the fixed day grid the date dialog renders: every day of
// October 2026.
var acOctoberDays = func() []int {
	days := make([]int, 31)
	for i := range days {
		days[i] = i + 1
	}
	return days
}()

func acTripLabel(trip string) string {
	if trip == "oneway" {
		return "Solo ida"
	}
	return "Ida y vuelta"
}

// acFormatDate turns the hidden field's "2026-10-15" into the visible
// "15/10/2026" the day-picker also writes into the input.
func acFormatDate(iso string) string {
	if len(iso) != len("2026-10-15") {
		return iso
	}
	return iso[8:10] + "/" + iso[5:7] + "/" + iso[0:4]
}

// acWidgetsJS wires the three widgets that only a click can drive:
//   - the two station comboboxes: on input it filters the fixed station
//     list by prefix (case- and accent-insensitive) and clears the hidden
//     committed id, since a raw keystroke is never a chosen station; only
//     clicking a rendered option sets the visible text, the hidden id and
//     closes the list;
//   - the trip-type combobox div: a click toggles its listbox open/closed;
//     clicking an option sets the div's text, the hidden "trip" input and
//     closes the list;
//   - the date dialog: focusing or clicking the readonly input opens it;
//     clicking a day button writes the visible and hidden date fields and
//     closes it; "Listo" only closes it.
const acWidgetsJS = `
(function(){
  var stations = [
    ["MAD-ATO","Madrid-Puerta de Atocha"],
    ["MAD-CHM","Madrid-Chamartín"],
    ["BCN-SAN","Barcelona-Sants"],
    ["VLC-JSO","Valencia-Joaquín Sorolla"],
    ["SEV-SJU","Sevilla-Santa Justa"],
    ["MAL-MZA","Málaga-María Zambrano"]
  ];
  function fold(s){
    return s.toLowerCase().normalize('NFD').replace(/[\u0300-\u036f]/g,'');
  }
  function wire(inputId, listId, hiddenId){
    var input = document.getElementById(inputId);
    var list = document.getElementById(listId);
    var hidden = document.getElementById(hiddenId);
    input.addEventListener('input', function(){
      hidden.value = '';
      var q = fold(input.value);
      list.innerHTML = '';
      if(q === ''){
        input.setAttribute('aria-expanded', 'false');
        return;
      }
      var matches = stations.filter(function(s){ return fold(s[1]).indexOf(q) === 0; });
      matches.forEach(function(s){
        var li = document.createElement('li');
        li.setAttribute('role', 'option');
        li.setAttribute('tabindex', '-1');
        li.dataset.code = s[0];
        li.textContent = s[1];
        list.appendChild(li);
      });
      input.setAttribute('aria-expanded', matches.length > 0 ? 'true' : 'false');
    });
    list.addEventListener('click', function(e){
      var li = e.target.closest('[role="option"]');
      if(!li) return;
      input.value = li.textContent;
      hidden.value = li.dataset.code;
      list.innerHTML = '';
      input.setAttribute('aria-expanded', 'false');
    });
  }
  wire('from-input','from-list','from_id');
  wire('to-input','to-list','to_id');

  var trip = document.getElementById('trip');
  var tripList = document.getElementById('trip-list');
  var tripHidden = document.getElementById('trip_hidden');
  trip.addEventListener('click', function(){
    var opening = tripList.hasAttribute('hidden');
    if(opening){
      tripList.removeAttribute('hidden');
      trip.setAttribute('aria-expanded', 'true');
    } else {
      tripList.setAttribute('hidden', '');
      trip.setAttribute('aria-expanded', 'false');
    }
  });
  tripList.addEventListener('click', function(e){
    var li = e.target.closest('[role="option"]');
    if(!li) return;
    trip.textContent = li.textContent;
    tripHidden.value = li.dataset.value;
    tripList.setAttribute('hidden', '');
    trip.setAttribute('aria-expanded', 'false');
  });

  var dateInput = document.getElementById('date');
  var cal = document.getElementById('cal');
  var dateHidden = document.getElementById('date_hidden');
  function openCal(){ cal.removeAttribute('hidden'); }
  dateInput.addEventListener('focus', openCal);
  dateInput.addEventListener('click', openCal);
  cal.addEventListener('click', function(e){
    var day = e.target.closest('button[data-day]');
    if(day){
      var dd = day.dataset.day.length === 1 ? '0' + day.dataset.day : day.dataset.day;
      dateInput.value = dd + '/10/2026';
      dateHidden.value = '2026-10-' + dd;
      cal.setAttribute('hidden', '');
      return;
    }
    if(e.target.id === 'cal-done'){
      cal.setAttribute('hidden', '');
    }
  });
})();
`

// acCookieJS dismisses the modal cookie banner: either button just records
// consent and removes the dialog and its click-blocking overlay, never
// anything the server needs to see.
const acCookieJS = `
(function(){
  var dialog = document.getElementById('cookies');
  var overlay = document.getElementById('cookies-overlay');
  function dismiss(){
    document.cookie = 'ck=1; path=/';
    if(dialog) dialog.remove();
    if(overlay) overlay.remove();
  }
  var accept = document.getElementById('ck-accept');
  var reject = document.getElementById('ck-reject');
  if(accept) accept.addEventListener('click', dismiss);
  if(reject) reject.addEventListener('click', dismiss);
})();
`

var (
	autocompleteFormTmpl = template.Must(template.New("autocomplete-form").Parse(head("Trainfinder") + `
{{if .ShowCookieBanner}}
<div id="cookies-overlay" style="position:fixed;inset:0;background:transparent;z-index:5"></div>
<div role="dialog" aria-modal="true" aria-label="Cookies" id="cookies"
  style="position:fixed;top:1rem;left:50%;transform:translateX(-50%);z-index:10;background:#fff">
  <p>Usamos cookies para recordar tu búsqueda. Puedes aceptarlas o rechazarlas.</p>
  <button type="button" id="ck-accept">Aceptar</button>
  <button type="button" id="ck-reject">Rechazar</button>
</div>
{{end}}
<h1>Find a train</h1>
{{if .Error}}<p role="alert" id="form-error">{{.Error}}</p>{{end}}
<form method="post" action="{{.Action}}">
  <label for="from-input">From</label>
  <input id="from-input" name="from" role="combobox" aria-autocomplete="list" aria-expanded="false"
    aria-controls="from-list" autocomplete="off" value="{{.From}}"
    {{if .FromInvalid}}aria-invalid="true" aria-errormessage="form-error"{{end}}>
  <ul role="listbox" id="from-list"></ul>
  <input type="hidden" id="from_id" name="from_id" value="{{.FromID}}">

  <label for="to-input">To</label>
  <input id="to-input" name="to" role="combobox" aria-autocomplete="list" aria-expanded="false"
    aria-controls="to-list" autocomplete="off" value="{{.To}}"
    {{if .ToInvalid}}aria-invalid="true" aria-errormessage="form-error"{{end}}>
  <ul role="listbox" id="to-list"></ul>
  <input type="hidden" id="to_id" name="to_id" value="{{.ToID}}">

  <label id="trip-label">Tipo de viaje</label>
  <div role="combobox" tabindex="0" aria-haspopup="listbox" aria-expanded="false"
    aria-controls="trip-list" aria-labelledby="trip-label" id="trip">{{.TripLabel}}</div>
  <ul role="listbox" id="trip-list" aria-label="Tipo de viaje" hidden>
    <li role="option" data-value="round">Ida y vuelta</li>
    <li role="option" data-value="oneway">Solo ida</li>
  </ul>
  <input type="hidden" id="trip_hidden" name="trip" value="{{.Trip}}">

  <label for="date">Fecha de salida</label>
  <input id="date" name="date_text" placeholder="Fecha de salida" autocomplete="off" readonly value="{{.DateText}}"
    {{if .DateInvalid}}aria-invalid="true" aria-errormessage="form-error"{{end}}>
  <div role="dialog" aria-label="Elige una fecha" id="cal" hidden>
    <p>Octubre 2026</p>
    {{range .Days}}<button type="button" data-day="{{.}}">{{.}}</button>
    {{end}}
    <button type="button" id="cal-done">Listo</button>
  </div>
  <input type="hidden" id="date_hidden" name="date" value="{{.Date}}">

  <button type="submit">Search</button>
</form>
<script>` + acWidgetsJS + acCookieJS + `</script>
` + foot))

	autocompleteResultsTmpl = template.Must(template.New("autocomplete-results").Parse(head("Trainfinder — Results") + `
<h1>Trains from {{.FromName}} to {{.ToName}} on {{.DateDisplay}} ({{.TripLabel}})</h1>
<table>
<caption>Ida</caption>
<tr><th>Departure</th><th>Price</th></tr>
{{range .Trains}}<tr><td>{{.Time}}</td><td>{{.Price}}{{if .Cheapest}} (cheapest){{end}}</td></tr>
{{end}}</table>
{{if .ReturnTrains}}
<table>
<caption>Vuelta</caption>
<tr><th>Departure</th><th>Price</th></tr>
{{range .ReturnTrains}}<tr><td>{{.Time}}</td><td>{{.Price}}{{if .Cheapest}} (cheapest){{end}}</td></tr>
{{end}}</table>
{{end}}
` + foot))
)

type autocompleteFormData struct {
	Action                              string
	From, To, FromID, ToID              string
	Trip, TripLabel                     string
	Date, DateText                      string
	Days                                []int
	Error                               string
	FromInvalid, ToInvalid, DateInvalid bool
	ShowCookieBanner                    bool
}

func registerAutocomplete(mux *http.ServeMux) {
	mux.HandleFunc("GET /autocomplete/", func(w http.ResponseWriter, r *http.Request) {
		showBanner := true
		if c, err := r.Cookie("ck"); err == nil && c.Value == "1" {
			showBanner = false
		}
		renderAutocompleteForm(w, http.StatusOK, runOf(r), autocompleteFormData{
			Trip: "round", TripLabel: acTripLabel("round"), ShowCookieBanner: showBanner,
		})
	})
	mux.HandleFunc("POST /autocomplete/search", autocompleteSearchHandler)
}

func autocompleteSearchHandler(w http.ResponseWriter, r *http.Request) {
	run := runOf(r)
	_ = r.ParseForm()
	from, to := r.PostFormValue("from"), r.PostFormValue("to")
	fromID, toID := r.PostFormValue("from_id"), r.PostFormValue("to_id")
	trip := r.PostFormValue("trip")
	if trip == "" {
		trip = "round"
	}
	date := r.PostFormValue("date")
	dateText := r.PostFormValue("date_text")

	fromName, fromOK := "", false
	if fromID != "" {
		fromName, fromOK = acStationName(fromID)
	}
	toName, toOK := "", false
	if toID != "" {
		toName, toOK = acStationName(toID)
	}

	if !fromOK || !toOK {
		renderAutocompleteForm(w, http.StatusBadRequest, run, autocompleteFormData{
			From: from, To: to, Trip: trip, TripLabel: acTripLabel(trip),
			Date: date, DateText: dateText,
			Error:       "Choose a station from the list for From and To.",
			FromInvalid: !fromOK, ToInvalid: !toOK,
		})
		return
	}

	if date == "" {
		renderAutocompleteForm(w, http.StatusBadRequest, run, autocompleteFormData{
			From: from, To: to, FromID: fromID, ToID: toID, Trip: trip, TripLabel: acTripLabel(trip),
			DateText:    dateText,
			Error:       "Elige una fecha en el calendario.",
			DateInvalid: true,
		})
		return
	}

	returnTrains := []acTrain(nil)
	if trip == "round" {
		returnTrains = acReturnTrains
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = autocompleteResultsTmpl.Execute(w, struct {
		FromName, ToName, DateDisplay, TripLabel string
		Trains, ReturnTrains                     []acTrain
	}{fromName, toName, acFormatDate(date), acTripLabel(trip), acTrains, returnTrains})
}

func renderAutocompleteForm(w http.ResponseWriter, status int, run string, data autocompleteFormData) {
	data.Action = withRun("/autocomplete/search", run)
	if data.Days == nil {
		data.Days = acOctoberDays
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = autocompleteFormTmpl.Execute(w, data)
}

func autocompleteSite() Site {
	return Site{
		Name:   "autocomplete",
		Path:   "/autocomplete/",
		Intent: "Find the cheapest one-way train from Madrid to Barcelona on 15 October 2026 on the autocomplete site and tell me its price.",
		Expect: "done, answer has 32,40",
		Answer: regexp.MustCompile("32,40"),
		Check:  func(run string) error { return nil },
	}
}
