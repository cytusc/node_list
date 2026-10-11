// Keep flags as separate assets so only countries visible on the page are fetched.
const assets = import.meta.glob<string>(
  "/node_modules/flag-icons/flags/4x3/*.svg",
  { eager: true, query: "?url", import: "default" },
);
export function flagUrl(code?: string) {
  return code
    ? assets[`/node_modules/flag-icons/flags/4x3/${code.toLowerCase()}.svg`]
    : undefined;
}
