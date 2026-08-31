// Client-side CSV export for the Users table (spec §9: users Export has no
// server endpoint — it is built from the rows already loaded on screen). No
// React/DOM: UsersTab triggers the actual download; this only shapes the text.

export interface UserCsvRow {
  username: string;
  email: string;
  role: string;
  /** "Active" | "Disabled" — the row's display state, not a raw enum. */
  state: string;
  activeSessionCount: number;
  /** Bytes, or null when the storage read hasn't resolved for this user. */
  homeBytes: number | null;
  /** ISO timestamp, or null when the user has never been seen. */
  lastSeenAt: string | null;
}

const HEADER = ["Username", "Email", "Role", "State", "Sessions", "Home bytes", "Last seen"];

/** RFC 4180: quote a field that contains a comma, quote or newline; double up
 *  any quotes inside it. */
function quote(value: string): string {
  return /[",\n]/.test(value) ? `"${value.replace(/"/g, '""')}"` : value;
}

export function usersToCsv(rows: UserCsvRow[]): string {
  const lines = [HEADER.join(",")];
  for (const r of rows) {
    lines.push(
      [
        r.username,
        r.email,
        r.role,
        r.state,
        String(r.activeSessionCount),
        r.homeBytes == null ? "" : String(r.homeBytes),
        r.lastSeenAt ?? "",
      ]
        .map(quote)
        .join(","),
    );
  }
  // A trailing newline makes the file end cleanly for tools that read line by
  // line; join, then add one more.
  return `${lines.join("\n")}\n`;
}
