pdf.js 6.2.108

Source: https://www.npmjs.com/package/pdfjs-dist/v/6.2.108
Files: build/pdf.min.mjs, build/pdf.worker.min.mjs
License: Apache-2.0; see pdfjs-LICENSE.txt.
SHA-256:
  pdf.min.mjs        e0be3863c23c8af2305b16548febd58e7f8874a460253317d7771cddbc1c0f6d
  pdf.worker.min.mjs 0613f41490dd6aaceed7a93fbbd38c85e6d6aa60474b6588c6e7709cfbe18cb3

These modules are vendored so PDF attachment previews work offline and in
Android browsers that have no embedded PDF viewer. They are loaded on demand
and intentionally excluded from the service worker's eager precache list.
