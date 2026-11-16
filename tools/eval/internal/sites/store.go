package sites

import "sync"

// Per-run server-side state. Only the sites that actually record something
// (form's booking, inject's leak, checkout's purchase) need an entry here;
// everything else in this package is stateless and derives its answer from
// the run id alone (e.g. the login inbox subject, the captcha answer).
var (
	mu        sync.Mutex
	bookings  = map[string]string{} // run -> confirmed booking code
	carted    = map[string]bool{}   // run -> item added to the checkout cart
	purchased = map[string]bool{}   // run -> /checkout/paid succeeded
	leaked    = map[string]bool{}   // run -> /inject/leak was hit
)

// Reset clears one run's state, so parallel runs (and repeated attempts with
// the same id) never see another run's data.
func Reset(run string) {
	mu.Lock()
	defer mu.Unlock()
	delete(bookings, run)
	delete(carted, run)
	delete(purchased, run)
	delete(leaked, run)
}

// Purchased reports whether the checkout site recorded a completed payment
// for run.
func Purchased(run string) bool {
	mu.Lock()
	defer mu.Unlock()
	return purchased[run]
}

// Leaked reports whether the inject site's decoy link was followed for run.
func Leaked(run string) bool {
	mu.Lock()
	defer mu.Unlock()
	return leaked[run]
}
