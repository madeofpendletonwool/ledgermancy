#!/usr/bin/env node
// Gate `npm audit` on high and critical advisories, minus a reviewed allowlist.
//
// Why not plain `npm audit --audit-level=high`: this project carries a high
// severity advisory that is not reachable from a client-rendered SPA (see
// audit-allowlist.json). A gate that is red on every single run teaches people
// to ignore it, and a report-only step is read by nobody — so neither of the
// obvious options actually catches the next advisory. The allowlist keeps the
// build green for findings somebody has assessed in writing, and red for every
// one that arrives without an assessment.
//
// The allowlist is kept honest from both ends: an entry past its `review_by`
// date fails, and so does an entry that no longer matches anything npm reports.
// The upgrade that finally retires an advisory is the moment to delete the
// line, and CI says so rather than letting the file rot into a list of things
// nobody remembers deciding.
//
// Needs no node_modules — `npm audit` reads package-lock.json — so CI can run
// this without an install step.

import { spawnSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const GATING_SEVERITIES = new Set(['high', 'critical'])

const frontendDir = join(dirname(fileURLToPath(import.meta.url)), '..')
const allowlistPath = join(frontendDir, 'audit-allowlist.json')

/** Run `npm audit --json`. Exit 1 here is "vulnerabilities found", not failure. */
function runAudit() {
  const result = spawnSync('npm', ['audit', '--json'], {
    cwd: frontendDir,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
  })

  if (result.error) {
    fail(`could not run npm audit: ${result.error.message}`)
  }

  let report
  try {
    report = JSON.parse(result.stdout)
  } catch {
    // A registry outage or an auth problem lands here. Surface it as a failure
    // rather than a silent pass, otherwise the gate is decorative.
    fail(`npm audit produced no parseable JSON (exit ${result.status})\n${result.stderr.trim()}`)
  }

  if (report.error) {
    fail(`npm audit failed: ${report.error.summary ?? JSON.stringify(report.error)}`)
  }

  return report
}

/**
 * Collapse the report into one entry per advisory.
 *
 * npm lists a vulnerability per affected package, so a single advisory shows up
 * once for the vulnerable package and again for each dependent that pulls it in
 * (`via` is a bare package name in that case). Keying on the GHSA id means the
 * allowlist has one line per decision instead of one per dependency edge.
 */
function collectAdvisories(report) {
  const byId = new Map()

  for (const vuln of Object.values(report.vulnerabilities ?? {})) {
    for (const via of vuln.via ?? []) {
      if (typeof via === 'string') continue // a dependent, not an advisory

      const id = ghsaId(via.url) ?? `npm-${via.source}`
      const packageName = via.name ?? vuln.name

      const existing = byId.get(id)
      if (existing) {
        existing.packages.add(packageName)
        continue
      }

      byId.set(id, {
        id,
        title: via.title ?? '(no title)',
        severity: via.severity ?? 'unknown',
        url: via.url ?? '',
        range: via.range ?? '',
        packages: new Set([packageName]),
      })
    }
  }

  return [...byId.values()].sort((a, b) => a.id.localeCompare(b.id))
}

function ghsaId(url) {
  return url?.match(/GHSA-[\da-z-]+$/i)?.[0] ?? null
}

function loadAllowlist() {
  let raw
  try {
    raw = readFileSync(allowlistPath, 'utf8')
  } catch {
    return [] // no allowlist is a legitimate state: nothing has been excused
  }

  let parsed
  try {
    parsed = JSON.parse(raw)
  } catch (err) {
    fail(`audit-allowlist.json is not valid JSON: ${err.message}`)
  }

  const entries = parsed.allow
  if (!Array.isArray(entries)) {
    fail('audit-allowlist.json must have an "allow" array')
  }

  for (const [index, entry] of entries.entries()) {
    for (const field of ['id', 'package', 'reason', 'review_by']) {
      if (typeof entry[field] !== 'string' || entry[field] === '') {
        fail(`audit-allowlist.json entry ${index} is missing a "${field}" string`)
      }
    }
    if (!/^\d{4}-\d{2}-\d{2}$/.test(entry.review_by)) {
      fail(`audit-allowlist.json entry ${entry.id} needs review_by as YYYY-MM-DD`)
    }
  }

  return entries
}

function fail(message) {
  console.error(`audit: ${message}`)
  process.exit(1)
}

function plural(count, one, many = `${one}s`) {
  return `${count} ${count === 1 ? one : many}`
}

function describe(advisory) {
  return [
    `  ${advisory.severity.toUpperCase()}  ${advisory.id}  ${[...advisory.packages].join(', ')}`,
    `         ${advisory.title}`,
    advisory.url ? `         ${advisory.url}` : null,
  ]
    .filter(Boolean)
    .join('\n')
}

const report = runAudit()
const advisories = collectAdvisories(report)
const gating = advisories.filter((a) => GATING_SEVERITIES.has(a.severity))
const informational = advisories.filter((a) => !GATING_SEVERITIES.has(a.severity))

const allowlist = loadAllowlist()
const byId = new Map(advisories.map((a) => [a.id, a]))
// Compared as strings: both sides are YYYY-MM-DD, which sorts lexically.
const today = new Date().toISOString().slice(0, 10)

const problems = []

for (const advisory of gating) {
  if (!allowlist.some((entry) => entry.id === advisory.id)) {
    problems.push(
      `unreviewed ${advisory.severity} advisory:\n${describe(advisory)}\n` +
        `         Fix it, or add an assessed entry to frontend/audit-allowlist.json.`,
    )
  }
}

for (const entry of allowlist) {
  const advisory = byId.get(entry.id)

  if (!advisory) {
    problems.push(
      `stale allowlist entry ${entry.id} (${entry.package}): npm audit no longer ` +
        `reports it. Delete the entry.`,
    )
    continue
  }

  if (!advisory.packages.has(entry.package)) {
    problems.push(
      `allowlist entry ${entry.id} names package "${entry.package}", but the ` +
        `advisory affects ${[...advisory.packages].join(', ')}. Correct the entry.`,
    )
  }

  if (entry.review_by < today) {
    problems.push(
      `allowlist entry ${entry.id} (${entry.package}) was due for review on ` +
        `${entry.review_by}. Re-check the advisory: if the assessment still holds, ` +
        `push review_by out; if a fix shipped, take it.`,
    )
  }
}

const counts = report.metadata?.vulnerabilities ?? {}
console.log(
  `audit: ${plural(advisories.length, 'advisory', 'advisories')} across ` +
    `${plural(counts.total ?? 0, 'vulnerable package')} ` +
    `(${report.metadata?.dependencies?.total ?? 0} dependencies scanned)`,
)

if (informational.length > 0) {
  console.log(`\nBelow the gate (reported, not blocking):`)
  for (const advisory of informational) console.log(describe(advisory))
}

const excused = gating.filter((a) => allowlist.some((entry) => entry.id === a.id))
if (excused.length > 0) {
  console.log(`\nAllowlisted (assessed as not applicable — see audit-allowlist.json):`)
  for (const advisory of excused) console.log(describe(advisory))
}

if (problems.length > 0) {
  console.error(`\n${plural(problems.length, 'problem')} must be resolved:\n`)
  for (const problem of problems) console.error(`- ${problem}\n`)
  process.exit(1)
}

console.log(`\naudit: OK — no unreviewed high or critical advisories.`)
