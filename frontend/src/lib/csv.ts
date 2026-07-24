// A small RFC-4180-ish CSV parser: handles quoted fields, commas and newlines
// inside quotes, escaped quotes (""), and CRLF. Good enough for bank exports;
// it is not a streaming parser. Returns rows of raw string cells, dropping
// fully-blank lines.
export function parseCsv(text: string): string[][] {
  const rows: string[][] = []
  let field = ''
  let row: string[] = []
  let inQuotes = false

  const endField = () => {
    row.push(field)
    field = ''
  }
  const endRow = () => {
    endField()
    rows.push(row)
    row = []
  }

  for (let i = 0; i < text.length; i++) {
    const c = text[i]
    if (inQuotes) {
      if (c === '"') {
        if (text[i + 1] === '"') {
          field += '"'
          i++
        } else {
          inQuotes = false
        }
      } else {
        field += c
      }
      continue
    }
    if (c === '"') inQuotes = true
    else if (c === ',') endField()
    else if (c === '\n') endRow()
    else if (c !== '\r') field += c
  }
  if (field !== '' || row.length > 0) endRow()

  return rows.filter((r) => r.some((cell) => cell.trim() !== ''))
}
