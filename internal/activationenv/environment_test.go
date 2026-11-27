package activationenv

import (
	"reflect"
	"testing"
)

func TestDirectRemovesEveryInheritedRouteAndIsIdempotent(t *testing.T) {
	input := []string{"PATH=fixture", "http_proxy=http://old", "HTTPS_PROXY=http://old", "ALL_PROXY=socks5://old", "no_proxy=localhost", "APPLESERVICES_NATIVE_HTTP_PROXY=http://old", "APPLESERVICES_SOCKS_PROXY=old:1", "APPLESERVICES_PASSWORD=fixture"}
	want := []string{"PATH=fixture", "APPLESERVICES_PASSWORD=fixture", "NO_PROXY=*"}
	got := Direct(input)
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(Direct(got), want) {
		t.Fatal("activation route inherited an override")
	}
}
