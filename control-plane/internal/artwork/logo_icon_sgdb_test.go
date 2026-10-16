package artwork

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSteamGridDBLogoIconByAppid(t *testing.T) {
	c, stub := newStubProvider(t, map[string]stubResponse{
		"/logos/steam/620": {200, `{"success":true,"data":[
			{"id":1,"score":1,"width":800,"height":300,"url":"https://cdn.example/low.png"},
			{"id":2,"score":9,"width":800,"height":300,"url":"https://cdn.example/logo.png"},
			{"id":3,"score":99,"width":800,"height":300,"nsfw":true,"url":"https://cdn.example/nsfw.png"}]}`},
		"/icons/steam/620": {200, `{"success":true,"data":[
			{"id":4,"score":9,"width":512,"height":256,"url":"https://cdn.example/wide.png"},
			{"id":5,"score":1,"width":256,"height":256,"url":"https://cdn.example/icon.png"}]}`},
	})
	logo, icon, err := c.LogoIcon(context.Background(), ExternalSourceSteam, "620")
	if err != nil {
		t.Fatal(err)
	}
	if logo != "https://cdn.example/logo.png" || icon != "https://cdn.example/icon.png" {
		t.Fatalf("logo %q icon %q", logo, icon)
	}
	if len(stub.requests) != 2 ||
		stub.requests[0] != "/logos/steam/620?"+sgdbLogoFilter || stub.requests[1] != "/icons/steam/620?"+sgdbIconFilter {
		t.Fatalf("requests %v", stub.requests)
	}
}

func TestSteamGridDBLogoIconByProviderRefAndNotFound(t *testing.T) {
	c, stub := newStubProvider(t, map[string]stubResponse{
		"/icons/game/42": {200, `{"success":true,"data":[{"id":1,"score":1,"width":64,"height":64,"url":"https://cdn.example/i.png"}]}`},
	})
	logo, icon, err := c.LogoIcon(context.Background(), "", "42")
	if err != nil || logo != "" || icon != "https://cdn.example/i.png" {
		t.Fatalf("logo %q icon %q err %v", logo, icon, err)
	}
	if !strings.HasPrefix(stub.requests[0], "/logos/game/42?") {
		t.Fatalf("requests %v", stub.requests)
	}
}

func TestSteamGridDBLogoIconRefusesBadRefsBeforeAnyRequest(t *testing.T) {
	c, stub := newStubProvider(t, nil)
	if _, _, err := c.LogoIcon(context.Background(), ExternalSourceSteam, "620/../x"); err == nil {
		t.Fatal("a non-appid joined the URL path")
	}
	if _, _, err := c.LogoIcon(context.Background(), "epic", "1"); !errors.Is(err, ErrUnsupportedExternalSource) {
		t.Fatalf("err %v", err)
	}
	if _, _, err := c.LogoIcon(context.Background(), "", " "); err == nil {
		t.Fatal("empty ref accepted")
	}
	if len(stub.requests) != 0 {
		t.Fatalf("requests %v", stub.requests)
	}
}

func TestSteamGridDBLogoIconProviderErrorIsAnError(t *testing.T) {
	c, _ := newStubProvider(t, map[string]stubResponse{"/logos/": {500, `{}`}})
	if _, _, err := c.LogoIcon(context.Background(), ExternalSourceSteam, "620"); err == nil {
		t.Fatal("a 500 read as no logo")
	}
}
