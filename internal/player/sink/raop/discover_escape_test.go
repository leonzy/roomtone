package raop

import "testing"

func TestEscapedServiceName(t *testing.T) {
	for _, name := range []string{`0005CD2FE4B9\@Cocoon\ Portable._raop._tcp.local.`, `0005CD2FE4B9\064Cocoon\032Portable._raop._tcp.local.`} {
		id, display := SplitServiceName(name)
		if id != "0005cd2fe4b9" || display != "Cocoon Portable" {
			t.Fatalf("%q: %q %q", name, id, display)
		}
	}
}
