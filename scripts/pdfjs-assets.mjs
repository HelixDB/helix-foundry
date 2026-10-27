// Copies the files pdf.js fetches as it needs them (CMaps for CJK text, the
// standard fonts, and the WebAssembly image decoders scans need) from
// pdfjs-dist into public/pdfjs/<version>/, where the Pages view
// (src/web/areas/documents/pdf.tsx) points it. Run on install.
import { cpSync, readFileSync, rmSync } from "node:fs";

const from = "node_modules/pdfjs-dist";
const { version } = JSON.parse(readFileSync(`${from}/package.json`, "utf8"));
rmSync("public/pdfjs", { recursive: true, force: true });
for (const dir of ["cmaps", "standard_fonts", "wasm"])
  cpSync(`${from}/${dir}`, `public/pdfjs/${version}/${dir}`, {
    recursive: true,
  });
