import * as fs from "fs";
import * as path from "path";

type JsonValue = string | number | boolean | null | JsonValue[] | { [k: string]: JsonValue };

const REPORT_DIR = path.resolve(__dirname, "..", "reports");
const REPORT_FILE = path.join(REPORT_DIR, "deployment-report.json");
const LEGACY_REPORT_FILE = path.resolve(__dirname, "..", "artifacts", "deployment-report.json");

function ensureDir() {
  if (!fs.existsSync(REPORT_DIR)) {
    fs.mkdirSync(REPORT_DIR, { recursive: true });
  }
}

export function readReport(): Record<string, JsonValue> {
  ensureDir();
  // One-time migration: if legacy report exists under artifacts and new report
  // does not exist yet, copy it into reports/.
  if (!fs.existsSync(REPORT_FILE) && fs.existsSync(LEGACY_REPORT_FILE)) {
    try {
      fs.copyFileSync(LEGACY_REPORT_FILE, REPORT_FILE);
    } catch {
      // ignore migration errors and fall back to empty report
    }
  }
  if (!fs.existsSync(REPORT_FILE)) {
    return {};
  }
  try {
    return JSON.parse(fs.readFileSync(REPORT_FILE, "utf8"));
  } catch {
    return {};
  }
}

export function writeReport(next: Record<string, JsonValue>) {
  ensureDir();
  fs.writeFileSync(REPORT_FILE, JSON.stringify(next, null, 2), "utf8");
}

export function upsertReport(section: string, data: Record<string, JsonValue>) {
  const report = readReport();
  const current = typeof report[section] === "object" && report[section] !== null ? (report[section] as Record<string, JsonValue>) : {};
  report[section] = { ...current, ...data };
  writeReport(report);
  return REPORT_FILE;
}

export function reportPath() {
  ensureDir();
  return REPORT_FILE;
}
