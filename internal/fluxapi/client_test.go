package fluxapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestListIPs_StripsPortsAndDedupes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":[
			{"ip":"10.0.0.1:16101","name":"node1","ports":[]},
			{"ip":"10.0.0.2","name":"node2","ports":[]},
			{"ip":"10.0.0.1:16101","name":"dup","ports":[]}
		]}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	got, err := c.ListIPs(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []string{"10.0.0.1", "10.0.0.2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestListIPs_EmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	got, err := c.ListIPsHelper(srv.URL, "x")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

// helper avoiding repeated New() calls in tests
type t1 struct{}

var c t1

func (t1) ListIPsHelper(base, app string) ([]string, error) {
	return New(base).ListIPs(context.Background(), app)
}
