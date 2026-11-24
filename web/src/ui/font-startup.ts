import { loadFontFaces } from "./font-faces";
import { startFontSync } from "./font-sync";

export function startFonts() {
  void loadFontFaces();
  startFontSync();
}
