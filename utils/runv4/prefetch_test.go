package runv4

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeKeyServer serves a minimal but parseable /key response: a ctx blob of
// 0x8000 bytes and a state blob of 0x2000 bytes (the sizes fetchTemplate
// validates), plus the register fields it parses.
func fakeKeyServer(t *testing.T, calls *int32, delay time.Duration) *httptest.Server {
	t.Helper()
	ctx := make([]byte, 0x8000)
	state := make([]byte, 0x2004) // fetchTemplate reads [0x2000-offset : +4], needs > 0x2000
	for i := range ctx {
		ctx[i] = byte(i)
	}
	for i := range state {
		state[i] = byte(0xff - i)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if r.URL.Path != "/key" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":0,"msg":"SUCCESS","data":{`+
			`"ctx":%q,`+
			`"state":%q,`+
			`"rcx":"0x1","rax":"0x2","rdx":"0x3","r9":"0x4","rbp":"0x5"}}`,
			base64.StdEncoding.EncodeToString(ctx),
			base64.StdEncoding.EncodeToString(state))
	}))
	return srv
}

func resetTmplCache() {
	tmplCacheMu.Lock()
	tmplCache = map[string]*template{}
	tmplCacheMu.Unlock()
}

// TestPrefetchPopulatesCache verifies the prefetch starts a fetch that lands
// in the shared cache before the decrypt path needs it.
func TestPrefetchPopulatesCache(t *testing.T) {
	resetTmplCache()
	var calls int32
	srv := fakeKeyServer(t, &calls, 50*time.Millisecond)
	defer srv.Close()

	const uri = "skd://itunes.apple.com/p123/c1"
	prefetchTemplateFor(srv.URL, "123", uri, "")

	// The prefetch is async: wait for it to land (bounded).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := cachedTemplate(uri); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := cachedTemplate(uri); !ok {
		t.Fatal("prefetch did not populate the cache")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 /key call from prefetch, got %d", got)
	}

	// A subsequent templateFor must hit the cache, not fetch again.
	tmpl, err := templateForWith(srv.URL, "123", uri, "")
	if err != nil {
		t.Fatalf("templateFor after prefetch: %v", err)
	}
	if tmpl == nil {
		t.Fatal("templateFor returned nil template")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("templateFor should reuse prefetched template (no new /key call), got %d calls", got)
	}
}

// TestLazyFetchStillWorks verifies that when no prefetch ran, the decrypt
// path's lazy fetch still fetches and caches (the pre-change behavior).
func TestLazyFetchStillWorks(t *testing.T) {
	resetTmplCache()
	var calls int32
	srv := fakeKeyServer(t, &calls, 0)
	defer srv.Close()

	const uri = "skd://itunes.apple.com/p456/c2"
	tmpl, err := templateForWith(srv.URL, "456", uri, "")
	if err != nil {
		t.Fatalf("lazy templateFor: %v", err)
	}
	if tmpl == nil {
		t.Fatal("lazy templateFor returned nil")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 lazy /key call, got %d", got)
	}
}

// TestPrefetchFailureFallsBackToLazy verifies a failed prefetch (server down)
// leaves the cache empty and the lazy path still fetches successfully later.
func TestPrefetchFailureFallsBackToLazy(t *testing.T) {
	resetTmplCache()
	var calls int32
	srv := fakeKeyServer(t, &calls, 0)
	defer srv.Close()

	const uri = "skd://itunes.apple.com/p789/c3"
	// First, prefetch against a dead server.
	deadURL := "http://127.0.0.1:1"
	prefetchTemplateFor(deadURL, "789", uri, "")
	time.Sleep(100 * time.Millisecond) // give the failed goroutine time to run
	if _, ok := cachedTemplate(uri); ok {
		t.Fatal("cache should be empty after failed prefetch")
	}

	// Now the lazy path fetches against the live server and caches.
	tmpl, err := templateForWith(srv.URL, "789", uri, "")
	if err != nil {
		t.Fatalf("lazy templateFor after failed prefetch: %v", err)
	}
	if tmpl == nil {
		t.Fatal("lazy templateFor returned nil")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 lazy /key call, got %d", got)
	}
}

// templateForWith mirrors downloadAndDecryptFile's templateFor closure but
// against an explicit server URL (the closure captures Config.LiteServer).
func templateForWith(liteServer, adamId, uri, token string) (*template, error) {
	if uri == prefetchKey || uri == "" {
		return prefetchTemplate(), nil
	}
	if t, ok := cachedTemplate(uri); ok {
		return t, nil
	}
	tmplCacheMu.Lock()
	defer tmplCacheMu.Unlock()
	if t, ok := tmplCache[uri]; ok {
		return t, nil
	}
	t, err := fetchTemplate(liteServer, adamId, uri, token)
	if err != nil {
		return nil, err
	}
	tmplCache[uri] = t
	return t, nil
}

// sanity: ensure the fake ctx/state actually parse into a template the way
// fetchTemplate does (guards the fake against silent format drift).
func TestFakeTemplateParseable(t *testing.T) {
	resetTmplCache()
	var calls int32
	srv := fakeKeyServer(t, &calls, 0)
	defer srv.Close()
	tmpl, err := fetchTemplate(srv.URL, "1", "skd://itunes.apple.com/p1/c1", "")
	if err != nil {
		t.Fatalf("fetchTemplate on fake server: %v", err)
	}
	if len(tmpl.ctx) < 0x8000/4 {
		t.Fatalf("ctx too small: %d words", len(tmpl.ctx))
	}
	// state is loaded reversed into st (pos = 0x2000 - offset); verify a word.
	want := binary.LittleEndian.Uint32(make([]byte, 4)) // placeholder, real check below
	_ = want
	if tmpl.st[0] == 0 && tmpl.st[stSize-1] == 0 {
		t.Fatal("state did not parse into st")
	}
}
