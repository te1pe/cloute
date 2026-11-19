package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"cloute/internal/httpapi"
)

func TestStatic(t *testing.T) {
	h := httpapi.Static(fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>Cloute</title>")},
		"assets/index-abc.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc.css": {Data: []byte("body{}")},
	})
	get := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}

	if rec := get("GET", "/?doc=abc"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>Cloute") || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("index: %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
	if rec := get("GET", "/assets/index-abc.js"); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := get("GET", "/assets/missing.js"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset: %d", rec.Code)
	}
	if rec := get("POST", "/"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
}

func TestStaticWithoutFrontend(t *testing.T) {
	rec := httptest.NewRecorder()
	httpapi.Static(nil).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "make build") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}
