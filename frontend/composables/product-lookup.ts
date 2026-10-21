import type { BarcodeProduct } from "~~/lib/api/types/data-contracts";

export function normalizeProductCode(raw: string): string {
  return raw.trim().replace(/[\s\p{Pd}]+/gu, "").toUpperCase();
}

export function pickLookedUpProduct(products: BarcodeProduct[] | null | undefined): BarcodeProduct | undefined {
  if (!products?.length) {
    return undefined;
  }
  return (
    products.find(product => product.item?.name?.trim() && product.imageBase64) ||
    products.find(product => product.item?.name?.trim()) ||
    products[0]
  );
}

export function productSourceLabel(product: BarcodeProduct): string {
  return (product.item.notes || "").replace(/^Source:\s*/, "").trim() || product.search_engine_name;
}

export function notesWithProductSource(notes: string, source: string): string {
  const alternate = notes.match(/^Alternate ISBN:\s*(\S+)/m)?.[1] ?? "";
  const kept = notes
    .split("\n")
    .map(line => line.trim())
    .filter(line => line !== "" && !line.startsWith("Alternate ISBN:") && !line.startsWith("Source:"));
  const lines = [...kept];
  if (source) {
    lines.unshift(`Source: ${source}`);
  }
  if (alternate) {
    lines.push(`Alternate ISBN: ${alternate}`);
  }
  return lines.join("\n");
}

export function clipText(value: string, max: number): string {
  return Array.from(value).slice(0, max).join("");
}
