# GeoIP: DBIP Country Lite

This directory holds `dbip-country-lite.mmdb`, the IP-to-country database used
by the product-analytics recorder (`internal/analytics`) to resolve a visitor's
country at request time. The file is embedded into the binary at build time
(`go:embed` in `../geo.go`), so there is no runtime download and no external
service dependency.

- Source: [db-ip.com free databases](https://db-ip.com/db/download/ip-to-country-lite)
- File: `dbip-country-lite-2026-08.mmdb` (gunzipped), retrieved 2026-08
- License: **Creative Commons Attribution 4.0 International** (CC BY 4.0)

> These sites' databases are licensed under the Creative Commons Attribution
> 4.0 International License. You are free to use them, provided you attribute
> DB-IP.com as the source. Full text: https://creativecommons.org/licenses/by/4.0/

To refresh the copy (DBIP publishes monthly), download the current month,
gunzip, and replace the file here:

```
curl -L -o dbip.mmdb.gz https://download.db-ip.com/free/dbip-country-lite-<yyyymm>.mmdb.gz
gunzip -f dbip.mmdb.gz && mv dbip.mmdb dbip-country-lite.mmdb
```

Privacy note: only the two-letter ISO country code and the first 8 bytes of
sha256(IP) are ever stored; the raw IP is discarded immediately after the
lookup (plan `docs/plans/product-analytics.md`, Decision 3).
