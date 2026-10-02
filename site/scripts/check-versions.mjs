#!/usr/bin/env node
/**
 * Check an assembled two-version Pages artifact (see .github/workflows/pages.yml).
 *
 *   node scripts/check-versions.mjs <artifact dir> [--root /quasar] [--edge edge]
 *
 * The artifact dir is what GitHub Pages serves at <root>/: the stable build at
 * its top, the edge build under <edge>/. For every HTML page it reads each
 * root-absolute href and src and checks:
 *
 *   escapes  an edge page linking into stable (or anything outside edge). The
 *            switcher is a <select>, not a link, so it is not counted. Fatal.
 *   broken   a link to a path with no file in the artifact. Fatal for edge,
 *            reported for stable unless --strict-stable, because stable is
 *            built from a release tag as it is and cannot be fixed after the fact.
 *
 * Exits 1 on a fatal finding. Prints a summary either way.
 */
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';

const args = process.argv.slice(2);
const opt = (name, fallback) => {
	const i = args.indexOf(name);
	return i === -1 ? fallback : args[i + 1];
};
const dir = args.find((a, i) => !a.startsWith('--') && !args[i - 1]?.startsWith('--'));
if (!dir) {
	console.error('usage: check-versions.mjs <artifact dir> [--root /quasar] [--edge edge] [--strict-stable]');
	process.exit(2);
}
const root = opt('--root', '/quasar').replace(/\/+$/, '');
const edgeDir = opt('--edge', 'edge');
const edgeBase = `${root}/${edgeDir}`;
const strictStable = args.includes('--strict-stable');

function* htmlFiles(base) {
	for (const name of readdirSync(base)) {
		const full = join(base, name);
		if (statSync(full).isDirectory()) yield* htmlFiles(full);
		else if (name.endsWith('.html')) yield full;
	}
}

/** Map a URL path under the root to a file in the artifact, or undefined. */
function resolve(path) {
	if (path !== root && !path.startsWith(`${root}/`)) return undefined;
	let rel = decodeURIComponent(path.slice(root.length));
	if (rel === '' || rel.endsWith('/')) rel += 'index.html';
	const file = join(dir, rel);
	if (existsSync(file) && statSync(file).isFile()) return file;
	if (existsSync(join(file, 'index.html'))) return join(file, 'index.html');
	return undefined;
}

const found = { edge: { pages: 0, links: 0, escapes: [], broken: [] }, stable: { pages: 0, links: 0, escapes: [], broken: [] } };
const ATTR = /\s(?:href|src)="([^"]*)"/g;

for (const file of htmlFiles(dir)) {
	const rel = file.slice(dir.length).replace(/^\/+/, '');
	const version = rel === edgeDir || rel.startsWith(`${edgeDir}/`) ? 'edge' : 'stable';
	const tally = found[version];
	tally.pages++;
	const html = readFileSync(file, 'utf8');
	for (const [, raw] of html.matchAll(ATTR)) {
		const url = raw.replace(/&amp;/g, '&');
		if (!url.startsWith('/') || url.startsWith('//')) continue;
		const path = url.replace(/[#?].*$/, '');
		tally.links++;
		if (version === 'edge' && path !== edgeBase && !path.startsWith(`${edgeBase}/`)) {
			tally.escapes.push(`${rel}: ${url}`);
			continue;
		}
		if (!resolve(path)) tally.broken.push(`${rel}: ${url}`);
	}
}

let fatal = false;
for (const [version, t] of Object.entries(found)) {
	const uniq = (list) => [...new Set(list)];
	console.log(`${version}: ${t.pages} pages, ${t.links} root-absolute links, ${t.escapes.length} escaping, ${t.broken.length} broken`);
	for (const line of uniq(t.escapes).slice(0, 40)) console.log(`  escapes  ${line}`);
	for (const line of uniq(t.broken).slice(0, 40)) console.log(`  broken   ${line}`);
	if (t.escapes.length > 0) fatal = true;
	if (t.broken.length > 0 && (version === 'edge' || strictStable)) fatal = true;
}
if (found.edge.pages === 0) {
	console.log(`no edge build under ${edgeDir}/`);
	fatal = true;
}
process.exit(fatal ? 1 : 0);
