# Local browser dependencies

These upstream browser distributions are embedded in the Go binary. No CDN or
frontend build is required at runtime. Preserve their license files on upgrades.

| File | Version | Source | License |
| --- | --- | --- | --- |
| marked.js | 18.1.0 | https://registry.npmjs.org/marked/-/marked-18.1.0.tgz (`lib/marked.umd.js`) | MIT, marked.LICENSE |
| purify.js | 3.4.16 | https://registry.npmjs.org/dompurify/-/dompurify-3.4.16.tgz (`dist/purify.min.js`) | Apache-2.0 OR MPL-2.0, purify.LICENSE |

Markdown rendering must always sanitize parser output before inserting it into
the document; see `../markdown.js`. Library files are unmodified upstream files.
