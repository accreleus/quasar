package library

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// valveFromFixture serves testdata/appdetails/<appid>.json, recorded from the live store
// endpoint, and records each request's query.
func valveFromFixture(t *testing.T, name string) (*httptest.Server, *[]string) {
	t.Helper()
	body, err := os.ReadFile("testdata/appdetails/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

func fetchFixture(t *testing.T, name, appid string) (AppDetail, bool, []string) {
	t.Helper()
	srv, queries := valveFromFixture(t, name)
	a := NewAppDetails(true, quietLogger())
	a.endpoint = srv.URL
	det, ok := a.Fetch(context.Background(), []string{appid})[appid]
	return det, ok, *queries
}

// The recorded `filters=basic` answer has no genres, developers, publishers or release_date,
// which is why the request names them.
func TestAppDetailsBasicFilterLacksStoreDetails(t *testing.T) {
	det, ok, _ := fetchFixture(t, "620_basic", "620")
	if !ok || !det.IsGame || det.ControllerSupport != "full" {
		t.Fatalf("basic answer: ok=%v %+v", ok, det)
	}
	if det.Genres != nil || det.Developer != "" || det.Publisher != "" || det.ReleaseDate != "" {
		t.Fatalf("filters=basic now carries store details, so the widened filter can go: %+v", det)
	}
}

func TestAppDetailsParsesStoreDetails(t *testing.T) {
	det, ok, queries := fetchFixture(t, "620", "620")
	if !ok {
		t.Fatal("recorded answer not consulted")
	}
	want := AppDetail{
		IsGame:            true,
		ShortDescription:  det.ShortDescription,
		Genres:            []string{"Action", "Adventure"},
		Developer:         "Valve",
		Publisher:         "Valve",
		ReleaseDate:       "18 Apr, 2011",
		ControllerSupport: "full",
	}
	if !reflect.DeepEqual(det, want) || det.ShortDescription == "" {
		t.Fatalf("got %+v\nwant %+v", det, want)
	}
	if len(queries) != 1 || !strings.Contains(queries[0], "filters=basic,genres,developers,publishers,release_date") ||
		!strings.Contains(queries[0], "l=english") {
		t.Fatalf("request query %v", queries)
	}
}

func TestAppDetailsJoinsCreditsAndKeepsAbsentControllerSupportEmpty(t *testing.T) {
	det, ok, _ := fetchFixture(t, "8930", "8930")
	if !ok {
		t.Fatal("recorded answer not consulted")
	}
	if det.Developer != "Firaxis Games, Aspyr (Mac), Aspyr (Linux)" || det.Publisher != "2K, Aspyr (Mac), Aspyr (Linux)" {
		t.Fatalf("credits %q / %q", det.Developer, det.Publisher)
	}
	if det.ControllerSupport != "" || det.ReleaseDate != "23 Sep, 2010" || !reflect.DeepEqual(det.Genres, []string{"Strategy"}) {
		t.Fatalf("%+v", det)
	}
}

func TestAppDetailsUnsuccessfulAnswerIsNotConsulted(t *testing.T) {
	if _, ok, _ := fetchFixture(t, "1", "1"); ok {
		t.Fatal(`{"success":false} must read as "not consulted"`)
	}
}

// A store-details field of an unexpected shape is dropped alone; type still decides the rung.
func TestStoreDetailsWireToleratesOddShapesAndBoundsText(t *testing.T) {
	w := storeDetailsWire{
		Genres:            []byte(`[{"description":" Action "},{"description":"Action"},{"description":""},{"description":"` + strings.Repeat("x", 100) + `"}]`),
		Developers:        []byte(`"Valve"`),
		Publishers:        []byte(`[" A ","","B"]`),
		ReleaseDate:       []byte(`"2011"`),
		ControllerSupport: []byte(`"Partial"`),
	}
	d := w.parse()
	if !reflect.DeepEqual(d.Genres, []string{"Action", strings.Repeat("x", maxGenreLen)}) {
		t.Fatalf("genres %q", d.Genres)
	}
	if d.Developer != "" || d.Publisher != "A, B" || d.ReleaseDate != "" || d.ControllerSupport != "partial" {
		t.Fatalf("%+v", d)
	}
	if got := (storeDetailsWire{ControllerSupport: []byte(`"none"`)}).parse(); got.ControllerSupport != "" {
		t.Fatalf("unknown controller value kept: %q", got.ControllerSupport)
	}
}
