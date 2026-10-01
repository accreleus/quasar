import test from 'node:test';
import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { docsVersion, counterpartPath, CHANNEL_BASES } from './docs-version.js';
import { markdownToHtml } from 'satteri';

import { rebase, rebaseRedirects, satteriBaseLinks } from './base-links.js';

test('an unset channel is a local build: stable root, no switcher', () => {
	assert.deepEqual(docsVersion({}), { channel: 'stable', base: '/quasar', switcher: false, stableLabel: '' });
});

test('each channel defaults to its published base', () => {
	assert.equal(docsVersion({ QUASAR_DOCS_CHANNEL: 'stable' }).base, CHANNEL_BASES.stable);
	assert.equal(docsVersion({ QUASAR_DOCS_CHANNEL: 'edge' }).base, '/quasar/edge');
	assert.equal(docsVersion({ QUASAR_DOCS_CHANNEL: 'edge' }).switcher, true);
});

test('QUASAR_DOCS_BASE overrides the base and is normalised', () => {
	assert.equal(docsVersion({ QUASAR_DOCS_CHANNEL: 'edge', QUASAR_DOCS_BASE: 'quasar/edge/' }).base, '/quasar/edge');
	assert.equal(docsVersion({ QUASAR_DOCS_BASE: '/preview' }).base, '/preview');
});

test('the stable label passes through', () => {
	assert.equal(docsVersion({ QUASAR_DOCS_CHANNEL: 'edge', QUASAR_DOCS_STABLE_LABEL: ' v0.3.0 ' }).stableLabel, 'v0.3.0');
});

test('an unknown channel is refused rather than built as stable', () => {
	assert.throws(() => docsVersion({ QUASAR_DOCS_CHANNEL: 'nightly' }), /stable" or "edge/);
});

test('counterpartPath keeps the page path across channels', () => {
	assert.equal(counterpartPath('/quasar/edge/install/podman/', '/quasar/edge', '/quasar'), '/quasar/install/podman/');
	assert.equal(counterpartPath('/quasar/install/podman/', '/quasar', '/quasar/edge'), '/quasar/edge/install/podman/');
	assert.equal(counterpartPath('/quasar/', '/quasar', '/quasar/edge'), '/quasar/edge/');
	assert.equal(counterpartPath('/quasar/edge', '/quasar/edge', '/quasar'), '/quasar/');
	assert.equal(counterpartPath('/elsewhere/', '/quasar', '/quasar/edge'), '/quasar/edge/');
});

test('rebase moves root links under the base and leaves everything else', () => {
	const r = (u) => rebase(u, '/quasar', '/quasar/edge');
	assert.equal(r('/quasar/install/podman/'), '/quasar/edge/install/podman/');
	assert.equal(r('/quasar/install/docker/#prepare-the-host'), '/quasar/edge/install/docker/#prepare-the-host');
	assert.equal(r('/quasar'), '/quasar/edge');
	assert.equal(r('/quasar/'), '/quasar/edge/');
	assert.equal(r('/quasar#x'), '/quasar/edge#x');
	assert.equal(r('/quasar/edge/admin/'), '/quasar/edge/admin/');
	assert.equal(r('/quasarish/'), '/quasarish/');
	assert.equal(r('https://accreleus.github.io/quasar/prepare-host.sh'), 'https://accreleus.github.io/quasar/prepare-host.sh');
	assert.equal(r('#anchor'), '#anchor');
	assert.equal(r('../relative/'), '../relative/');
	assert.equal(rebase('/quasar/x/', '/quasar', '/quasar'), '/quasar/x/');
});

test('the Sätteri plugin rewrites rendered Markdown links', () => {
	const md = [
		'[Podman](/quasar/install/podman/) and [prep](/quasar/install/docker/#prepare-the-host)',
		'[home](/quasar/) [already](/quasar/edge/admin/) [out](https://accreleus.github.io/quasar/prepare-host.sh)',
		'![shot](/quasar/x.png) [anchor](#here)',
	].join('\n\n');
	const { html } = markdownToHtml(md, { hastPlugins: [satteriBaseLinks({ root: '/quasar', base: '/quasar/edge/' })] });
	assert.match(html, /href="\/quasar\/edge\/install\/podman\/"/);
	assert.match(html, /href="\/quasar\/edge\/install\/docker\/#prepare-the-host"/);
	assert.match(html, /href="\/quasar\/edge\/"/);
	assert.match(html, /href="\/quasar\/edge\/admin\/"/);
	assert.match(html, /href="https:\/\/accreleus\.github\.io\/quasar\/prepare-host\.sh"/);
	assert.match(html, /src="\/quasar\/edge\/x\.png"/);
	assert.match(html, /href="#here"/);
	assert.doesNotMatch(html, /"\/quasar\/(?!edge\/)/);
});

test('redirect targets follow the base', () => {
	assert.deepEqual(rebaseRedirects({ '/install/install/': '/quasar/install/docker/' }, '/quasar', '/quasar/edge'), {
		'/install/install/': '/quasar/edge/install/docker/',
	});
});

// The base-links plugin only sees Markdown and MDX. A component that writes the
// root into its markup would send edge readers to stable, so components build
// their links from import.meta.env.BASE_URL instead.
test('no component hard-codes the published root in a link', () => {
	const dir = fileURLToPath(new URL('../components/', import.meta.url));
	const offenders = [];
	for (const name of readdirSync(dir)) {
		const text = readFileSync(dir + name, 'utf8');
		text.split('\n').forEach((line, i) => {
			if (/(href|src)=\\?["'`]\/quasar[/"'`#?]/.test(line)) offenders.push(`${name}:${i + 1}`);
		});
	}
	assert.deepEqual(offenders, []);
});
