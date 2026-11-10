# Vendored front-end libraries

Embedded into the binary so the app works without any CDN. Files are
byte-for-byte copies from the npm tarballs; each tarball was checked
against the registry's sha512 `dist.integrity` before extracting.

| File | Package | Source file | License | sha256 |
| --- | --- | --- | --- | --- |
| `htmx-2.0.11.min.js` | htmx.org 2.0.11 | `dist/htmx.min.js` | 0BSD | `d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717` |
| `chart-4.5.1.umd.min.js` | chart.js 4.5.1 | `dist/chart.umd.min.js` | MIT, see `chart.js-LICENSE.md` | `48444a82d4edcb5bec0f1965faacdde18d9c17db3063d042abada2f705c9f54a` |

To upgrade: download `https://registry.npmjs.org/<package>/-/<package>-<version>.tgz`,
verify it against `dist.integrity` from `https://registry.npmjs.org/<package>/<version>`,
copy the file under a new versioned name and update the script tags in
`templates/base.html`.
