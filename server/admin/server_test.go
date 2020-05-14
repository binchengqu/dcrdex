// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package admin

import (
	"context"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/server/market"
	"github.com/decred/dcrd/certgen"
	"github.com/decred/slog"
	"github.com/go-chi/chi"
)

func init() {
	log = slog.NewBackend(os.Stdout).Logger("TEST")
	log.SetLevel(slog.LevelTrace)
}

type TMarket struct {
	running bool
	ep0, ep int64
	dur     uint64
	suspend *market.SuspendEpoch
	persist bool
}

type TCore struct {
	markets map[string]*TMarket
}

func (c *TCore) ConfigMsg() json.RawMessage { return nil }

func (c *TCore) Suspend(tSusp time.Time, persistBooks bool) map[string]*market.SuspendEpoch {
	return nil
}

func (c *TCore) SuspendMarket(name string, tSusp time.Time, persistBooks bool) *market.SuspendEpoch {
	tMkt := c.markets[name]
	if tMkt == nil {
		return nil
	}
	tMkt.persist = persistBooks
	tMkt.suspend.Idx = encode.UnixMilli(tSusp)
	tMkt.suspend.End = tSusp.Add(time.Millisecond)
	return tMkt.suspend
}

func (c *TCore) market(name string) *TMarket {
	if c.markets == nil {
		return nil
	}
	return c.markets[name]
}

func (c *TCore) MarketStatus(mktName string) *market.Status {
	mkt := c.market(mktName)
	if mkt == nil {
		return nil
	}
	var suspendEpoch int64
	if mkt.suspend != nil {
		suspendEpoch = mkt.suspend.Idx
	}
	return &market.Status{
		Running:       mkt.running,
		EpochDuration: mkt.dur,
		ActiveEpoch:   mkt.ep,
		StartEpoch:    mkt.ep0,
		SuspendEpoch:  suspendEpoch,
		PersistBook:   mkt.persist,
	}
}

func (c *TCore) MarketStatuses() map[string]*market.Status {
	mktStatuses := make(map[string]*market.Status, len(c.markets))
	for name, mkt := range c.markets {
		var suspendEpoch int64
		if mkt.suspend != nil {
			suspendEpoch = mkt.suspend.Idx
		}
		mktStatuses[name] = &market.Status{
			Running:       mkt.running,
			EpochDuration: mkt.dur,
			ActiveEpoch:   mkt.ep,
			StartEpoch:    mkt.ep0,
			SuspendEpoch:  suspendEpoch,
			PersistBook:   mkt.persist,
		}
	}
	return mktStatuses
}

func (c *TCore) MarketRunning(mktName string) (found, running bool) {
	mkt := c.market(mktName)
	if mkt == nil {
		return
	}
	return true, mkt.running
}

type tResponseWriter struct {
	b    []byte
	code int
}

func (w *tResponseWriter) Header() http.Header {
	return make(http.Header)
}
func (w *tResponseWriter) Write(msg []byte) (int, error) {
	w.b = msg
	return len(msg), nil
}
func (w *tResponseWriter) WriteHeader(statusCode int) {
	w.code = statusCode
}

// genCertPair generates a key/cert pair to the paths provided.
func genCertPair(certFile, keyFile string) error {
	log.Infof("Generating TLS certificates...")

	org := "dcrdex autogenerated cert"
	validUntil := time.Now().Add(10 * 365 * 24 * time.Hour)
	cert, key, err := certgen.NewTLSCertPair(elliptic.P521(), org,
		validUntil, nil)
	if err != nil {
		return err
	}

	// Write cert and key files.
	if err = ioutil.WriteFile(certFile, cert, 0644); err != nil {
		return err
	}
	if err = ioutil.WriteFile(keyFile, key, 0600); err != nil {
		os.Remove(certFile)
		return err
	}

	log.Infof("Done generating TLS certificates")
	return nil
}

var tPort = 5555

// If start is true, the Server's Run goroutine is started, and the shutdown
// func must be called when finished with the Server.
func newTServer(t *testing.T, start bool, authSHA [32]byte) (*Server, func()) {
	tmp, err := ioutil.TempDir("", "admin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	cert, key := filepath.Join(tmp, "tls.cert"), filepath.Join(tmp, "tls.key")
	err = genCertPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}

	s, err := NewServer(&SrvConfig{
		Core:    new(TCore),
		Addr:    fmt.Sprintf("localhost:%d", tPort),
		Cert:    cert,
		Key:     key,
		AuthSHA: authSHA,
	})
	if err != nil {
		t.Fatalf("error creating Server: %v", err)
	}
	if !start {
		return s, func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		s.Run(ctx)
		wg.Done()
	}()
	shutdown := func() {
		cancel()
		wg.Wait()
	}
	return s, shutdown
}

func TestPing(t *testing.T) {
	w := httptest.NewRecorder()
	// apiPing is a Server method, but the receiver and http.Request are unused.
	(*Server)(nil).apiPing(w, nil)
	if w.Code != 200 {
		t.Fatalf("apiPing returned code %d, expected 200", w.Code)
	}

	resp := w.Result()
	ctHdr := resp.Header.Get("Content-Type")
	wantCt := "application/json; charset=utf-8"
	if ctHdr != wantCt {
		t.Errorf("Content-Type incorrect. got %q, expected %q", ctHdr, wantCt)
	}

	// JSON strings are double quoted. Each value is terminated with a newline.
	expectedBody := `"pong"` + "\n"
	if w.Body == nil {
		t.Fatalf("got empty body")
	}
	gotBody := w.Body.String()
	if gotBody != expectedBody {
		t.Errorf("apiPong response said %q, expected %q", gotBody, expectedBody)
	}
}

func TestMarkets(t *testing.T) {
	core := &TCore{
		markets: make(map[string]*TMarket),
	}
	srv := &Server{
		core: core,
	}

	mux := chi.NewRouter()
	mux.Get("/markets", srv.apiMarkets)

	// No markets.
	w := httptest.NewRecorder()
	r, _ := http.NewRequest("GET", "https://localhost/markets", nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("apiMarkets returned code %d, expected %d", w.Code, http.StatusOK)
	}
	respBody := w.Body.String()
	if respBody != fmt.Sprintf("{}\n") {
		t.Errorf("incorrect response body: %q", respBody)
	}

	// A market.
	dur := uint64(1234)
	idx := int64(12345)
	tMkt := &TMarket{
		running: true,
		dur:     dur,
		ep0:     12340,
		ep:      12343,
	}
	core.markets["dcr_btc"] = tMkt

	w = httptest.NewRecorder()
	r, _ = http.NewRequest("GET", "https://localhost/markets", nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("apiMarkets returned code %d, expected %d", w.Code, http.StatusOK)
	}

	exp := `{
    "dcr_btc": {
        "running": true,
        "epochlen": 1234,
        "activeepoch": 12343,
        "startepoch": 12340
    }
}
`
	if exp != w.Body.String() {
		t.Errorf("unexpected response %q, wanted %q", w.Body.String(), exp)
	}

	var mktStatuses map[string]*MarketStatus
	err := json.Unmarshal(w.Body.Bytes(), &mktStatuses)
	if err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	wantMktStatuses := map[string]*MarketStatus{
		"dcr_btc": {
			Running:       true,
			EpochDuration: 1234,
			ActiveEpoch:   12343,
			StartEpoch:    12340,
		},
	}
	if len(wantMktStatuses) != len(mktStatuses) {
		t.Fatalf("got %d market statuses, wanted %d", len(mktStatuses), len(wantMktStatuses))
	}
	for name, stat := range mktStatuses {
		wantStat := wantMktStatuses[name]
		if wantStat == nil {
			t.Fatalf("market %s not expected", name)
		}
		if !reflect.DeepEqual(wantStat, stat) {
			log.Errorf("incorrect market status. got %v, expected %v", stat, wantStat)
		}
	}

	// Set suspend data.
	tMkt.suspend = &market.SuspendEpoch{Idx: 12345, End: encode.UnixTimeMilli(int64(dur) * idx)}
	tMkt.persist = true

	w = httptest.NewRecorder()
	r, _ = http.NewRequest("GET", "https://localhost/markets", nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("apiMarkets returned code %d, expected %d", w.Code, http.StatusOK)
	}

	exp = `{
    "dcr_btc": {
        "running": true,
        "epochlen": 1234,
        "activeepoch": 12343,
        "startepoch": 12340,
        "finalepoch": 12345,
        "persistbook": true
    }
}
`
	if exp != w.Body.String() {
		t.Errorf("unexpected response %q, wanted %q", w.Body.String(), exp)
	}

	mktStatuses = nil
	err = json.Unmarshal(w.Body.Bytes(), &mktStatuses)
	if err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	persist := true
	wantMktStatuses = map[string]*MarketStatus{
		"dcr_btc": {
			Running:       true,
			EpochDuration: 1234,
			ActiveEpoch:   12343,
			StartEpoch:    12340,
			SuspendEpoch:  12345,
			PersistBook:   &persist,
		},
	}
	if len(wantMktStatuses) != len(mktStatuses) {
		t.Fatalf("got %d market statuses, wanted %d", len(mktStatuses), len(wantMktStatuses))
	}
	for name, stat := range mktStatuses {
		wantStat := wantMktStatuses[name]
		if wantStat == nil {
			t.Fatalf("market %s not expected", name)
		}
		if !reflect.DeepEqual(wantStat, stat) {
			log.Errorf("incorrect market status. got %v, expected %v", stat, wantStat)
		}
	}
}

func TestMarketInfo(t *testing.T) {

	core := &TCore{
		markets: make(map[string]*TMarket),
	}
	srv := &Server{
		core: core,
	}

	mux := chi.NewRouter()
	mux.Get("/market/{"+marketNameKey+"}", srv.apiMarketInfo)

	// Request a non-existent market.
	w := httptest.NewRecorder()
	name := "dcr_btc"
	r, _ := http.NewRequest("GET", "https://localhost/market/"+name, nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("apiMarketInfo returned code %d, expected %d", w.Code, http.StatusBadRequest)
	}
	respBody := w.Body.String()
	if respBody != fmt.Sprintf("unknown market %q\n", name) {
		t.Errorf("incorrect response body: %q", respBody)
	}

	tMkt := &TMarket{}
	core.markets[name] = tMkt

	// Not running market.
	w = httptest.NewRecorder()
	r, _ = http.NewRequest("GET", "https://localhost/market/"+name, nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("apiMarketInfo returned code %d, expected %d", w.Code, http.StatusOK)
	}
	mktStatus := new(MarketStatus)
	err := json.Unmarshal(w.Body.Bytes(), &mktStatus)
	if err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}
	if mktStatus.Name != name {
		t.Errorf("incorrect market name %q, expected %q", mktStatus.Name, name)
	}
	if mktStatus.Running {
		t.Errorf("market should not have been reported as running")
	}

	// Flip the market on.
	core.markets[name].running = true
	core.markets[name].suspend = &market.SuspendEpoch{Idx: 1324, End: time.Now()}
	w = httptest.NewRecorder()
	r, _ = http.NewRequest("GET", "https://localhost/market/"+name, nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("apiMarketInfo returned code %d, expected %d", w.Code, http.StatusOK)
	}
	mktStatus = new(MarketStatus)
	err = json.Unmarshal(w.Body.Bytes(), &mktStatus)
	if err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}
	if mktStatus.Name != name {
		t.Errorf("incorrect market name %q, expected %q", mktStatus.Name, name)
	}
	if !mktStatus.Running {
		t.Errorf("market should have been reported as running")
	}
}

func TestSuspend(t *testing.T) {

	core := &TCore{
		markets: make(map[string]*TMarket),
	}
	srv := &Server{
		core: core,
	}

	mux := chi.NewRouter()
	mux.Get("/market/{"+marketNameKey+"}/suspend", srv.apiSuspend)

	// Non-existent market
	name := "dcr_btc"
	w := httptest.NewRecorder()
	r, _ := http.NewRequest("GET", "https://localhost/market/"+name+"/suspend", nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("apiSuspend returned code %d, expected %d", w.Code, http.StatusBadRequest)
	}

	// With the market, but not running
	tMkt := &TMarket{
		suspend: &market.SuspendEpoch{},
	}
	core.markets[name] = tMkt

	w = httptest.NewRecorder()
	r, _ = http.NewRequest("GET", "https://localhost/market/"+name+"/suspend", nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("apiSuspend returned code %d, expected %d", w.Code, http.StatusOK)
	}
	wantMsg := "market \"dcr_btc\" not running\n"
	if w.Body.String() != wantMsg {
		t.Errorf("expected body %q, got %q", wantMsg, w.Body)
	}

	// Now running.
	tMkt.running = true
	w = httptest.NewRecorder()
	r, _ = http.NewRequest("GET", "https://localhost/market/"+name+"/suspend", nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("apiSuspend returned code %d, expected %d", w.Code, http.StatusOK)
	}
	suspRes := new(SuspendResult)
	err := json.Unmarshal(w.Body.Bytes(), &suspRes)
	if err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}
	if suspRes.Market != name {
		t.Errorf("incorrect market name %q, expected %q", suspRes.Market, name)
	}

	var zeroTime time.Time
	wantIdx := encode.UnixMilli(zeroTime)
	if suspRes.FinalEpoch != wantIdx {
		t.Errorf("incorrect final epoch index. got %d, expected %d",
			suspRes.FinalEpoch, tMkt.suspend.Idx)
	}

	wantFinal := zeroTime.Add(time.Millisecond)
	if suspRes.SuspendTime.Equal(wantFinal) {
		t.Errorf("incorrect suspend time. got %v, expected %v",
			suspRes.SuspendTime, tMkt.suspend.End)
	}

	// Specify a time in the past.
	w = httptest.NewRecorder()
	r, _ = http.NewRequest("GET", "https://localhost/market/"+name+"/suspend?t=12", nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("apiSuspend returned code %d, expected %d", w.Code, http.StatusOK)
	}
	resp := w.Body.String()
	wantPrefix := "specified market suspend time is in the past"
	if !strings.HasPrefix(resp, wantPrefix) {
		t.Errorf("Expected error message starting with %q, got %q", wantPrefix, resp)
	}

	// Bad suspend time (not a time)
	w = httptest.NewRecorder()
	r, _ = http.NewRequest("GET", "https://localhost/market/"+name+"/suspend?t=QWERT", nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("apiSuspend returned code %d, expected %d", w.Code, http.StatusOK)
	}
	resp = w.Body.String()
	wantPrefix = "invalid suspend time"
	if !strings.HasPrefix(resp, wantPrefix) {
		t.Errorf("Expected error message starting with %q, got %q", wantPrefix, resp)
	}

	// Good suspend time, one minute in the future
	w = httptest.NewRecorder()
	tMsFuture := encode.UnixMilli(time.Now().Add(time.Minute))
	r, _ = http.NewRequest("GET", fmt.Sprintf("https://localhost/market/%v/suspend?t=%d", name, tMsFuture), nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("apiSuspend returned code %d, expected %d", w.Code, http.StatusOK)
	}
	suspRes = new(SuspendResult)
	err = json.Unmarshal(w.Body.Bytes(), &suspRes)
	if err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if suspRes.FinalEpoch != tMsFuture {
		t.Errorf("incorrect final epoch index. got %d, expected %d",
			suspRes.FinalEpoch, tMsFuture)
	}

	wantFinal = encode.UnixTimeMilli(tMsFuture + 1)
	if suspRes.SuspendTime.Equal(wantFinal) {
		t.Errorf("incorrect suspend time. got %v, expected %v",
			suspRes.SuspendTime, wantFinal)
	}

	if !tMkt.persist {
		t.Errorf("market persist was false")
	}

	// persist=true (OK)
	w = httptest.NewRecorder()
	r, _ = http.NewRequest("GET", "https://localhost/market/"+name+"/suspend?persist=true", nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("apiSuspend returned code %d, expected %d", w.Code, http.StatusOK)
	}

	if !tMkt.persist {
		t.Errorf("market persist was false")
	}

	// persist=0 (OK)
	w = httptest.NewRecorder()
	r, _ = http.NewRequest("GET", "https://localhost/market/"+name+"/suspend?persist=0", nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("apiSuspend returned code %d, expected %d", w.Code, http.StatusOK)
	}

	if tMkt.persist {
		t.Errorf("market persist was true")
	}

	// invalid persist
	w = httptest.NewRecorder()
	r, _ = http.NewRequest("GET", "https://localhost/market/"+name+"/suspend?persist=blahblahblah", nil)
	r.RemoteAddr = "localhost"

	mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("apiSuspend returned code %d, expected %d", w.Code, http.StatusOK)
	}
	resp = w.Body.String()
	wantPrefix = "invalid persist book boolean"
	if !strings.HasPrefix(resp, wantPrefix) {
		t.Errorf("Expected error message starting with %q, got %q", wantPrefix, resp)
	}
}

func TestAuthMiddleware(t *testing.T) {
	pass := "password123"
	authSHA := sha256.Sum256([]byte(pass))
	s, _ := newTServer(t, false, authSHA)
	am := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r, _ := http.NewRequest("GET", "", nil)
	r.RemoteAddr = "localhost"

	wantAuthError := func(name string, want bool) {
		w := &tResponseWriter{}
		am.ServeHTTP(w, r)
		if w.code != http.StatusUnauthorized && w.code != http.StatusOK {
			t.Fatalf("unexpected HTTP error %d for test \"%s\"", w.code, name)
		}
		switch want {
		case true:
			if w.code != http.StatusUnauthorized {
				t.Fatalf("Expected unauthorized HTTP error for test \"%s\"", name)
			}
		case false:
			if w.code != http.StatusOK {
				t.Fatalf("Expected OK HTTP status for test \"%s\"", name)
			}
		}
	}

	tests := []struct {
		name, user, pass string
		wantErr          bool
	}{{
		name: "user and correct password",
		user: "user",
		pass: pass,
	}, {
		name: "only correct password",
		pass: pass,
	}, {
		name:    "only user",
		user:    "user",
		wantErr: true,
	}, {
		name:    "no user or password",
		wantErr: true,
	}, {
		name:    "wrong password",
		user:    "user",
		pass:    pass[1:],
		wantErr: true,
	}}
	for _, test := range tests {
		r.SetBasicAuth(test.user, test.pass)
		wantAuthError(test.name, test.wantErr)
	}
}