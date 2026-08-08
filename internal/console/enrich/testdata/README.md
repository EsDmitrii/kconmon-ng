# Enrichment mmdb fixtures

Two hand-built MaxMind DB files, ~1.7 KB each, used by `enrich_test.go` to
exercise the real `maxminddb.Open` + `Lookup` path without shipping a GeoLite2
database (they are licensed) and without touching the network.

| file | database type | networks |
| --- | --- | --- |
| `GeoLite2-ASN-Test.mmdb` | `GeoLite2-ASN` | `192.0.2.0/25` → AS64496 "Example Transit Network"; `192.0.2.128/26` → AS64497 "Example Edge Network"; `2001:db8::/64` → AS64500 "Example IPv6 Network" |
| `GeoLite2-City-Test.mmdb` | `GeoLite2-City` | `192.0.2.0/25` → GB / London / 51.5074, -0.1278; `192.0.2.128/26` → US / Ashburn / 39.0438, -77.4874; `2001:db8::/64` → DE / Frankfurt / 50.1109, 8.6821 |

Every address is from a documentation range: `192.0.2.0/24` is TEST-NET-1
(RFC 5737) and `2001:db8::/32` is the RFC 3849 documentation prefix. The ASNs
are RFC 5398 documentation ASNs. `192.0.2.192/26` is deliberately absent from
both files — that is the fixture's "address the source knows nothing about"
case, which is what makes the `miss` result testable.

## Regenerating them

The generator is NOT part of this module and must stay that way.
`github.com/maxmind/mmdbwriter` is the only practical way to write an mmdb
file, and M5's dependency budget is EXACTLY ONE new module
(`github.com/oschwald/maxminddb-golang/v2`, Decision 5) — so the writer lives
in a throwaway module outside the repo instead of in `go.mod`. Fixtures this
small and this static are cheaper to commit than to rebuild on every `go test`.

```console
$ mkdir /tmp/mmdbgen && cd /tmp/mmdbgen
$ go mod init mmdbgen
$ go get github.com/maxmind/mmdbwriter@v1.2.0
$ cat > main.go <<'EOF'
package main

import (
	"log"
	"net"
	"os"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		log.Fatal(err)
	}
	return n
}

func write(w *mmdbwriter.Tree, path string) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if _, err := w.WriteTo(f); err != nil {
		log.Fatal(err)
	}
}

func main() {
	// IncludeReservedNetworks is REQUIRED: mmdbwriter refuses to insert into
	// TEST-NET-1 without it, and TEST-NET-1 is exactly what these fixtures use.
	asn, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "GeoLite2-ASN",
		RecordSize:              24,
		IncludeReservedNetworks: true,
		Description:             map[string]string{"en": "kconmon-ng enrichment test fixture (TEST-NET-1 only)"},
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, e := range []struct {
		cidr string
		num  uint32
		org  string
	}{
		{"192.0.2.0/25", 64496, "Example Transit Network"},
		{"192.0.2.128/26", 64497, "Example Edge Network"},
		{"2001:db8::/64", 64500, "Example IPv6 Network"},
	} {
		if err := asn.Insert(mustCIDR(e.cidr), mmdbtype.Map{
			"autonomous_system_number":       mmdbtype.Uint32(e.num),
			"autonomous_system_organization": mmdbtype.String(e.org),
		}); err != nil {
			log.Fatal(err)
		}
	}
	write(asn, "GeoLite2-ASN-Test.mmdb")

	city, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "GeoLite2-City",
		RecordSize:              24,
		IncludeReservedNetworks: true,
		Description:             map[string]string{"en": "kconmon-ng enrichment test fixture (TEST-NET-1 only)"},
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, e := range []struct {
		cidr, iso, city string
		lat, lon        float64
	}{
		{"192.0.2.0/25", "GB", "London", 51.5074, -0.1278},
		{"192.0.2.128/26", "US", "Ashburn", 39.0438, -77.4874},
		{"2001:db8::/64", "DE", "Frankfurt", 50.1109, 8.6821},
	} {
		if err := city.Insert(mustCIDR(e.cidr), mmdbtype.Map{
			"country": mmdbtype.Map{"iso_code": mmdbtype.String(e.iso)},
			"city":    mmdbtype.Map{"names": mmdbtype.Map{"en": mmdbtype.String(e.city)}},
			"location": mmdbtype.Map{
				"latitude":  mmdbtype.Float64(e.lat),
				"longitude": mmdbtype.Float64(e.lon),
			},
		}); err != nil {
			log.Fatal(err)
		}
	}
	write(city, "GeoLite2-City-Test.mmdb")
}
EOF
$ go run .
$ cp *.mmdb <repo>/internal/console/enrich/testdata/
$ cd / && rm -rf /tmp/mmdbgen
```

There is deliberately no "corrupt database" fixture here: the test that proves
an unreadable file degrades to a disabled source writes its own garbage bytes
into `t.TempDir()`.
