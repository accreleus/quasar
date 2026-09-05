// @ts-check
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import starlightOpenAPI, { createOpenAPISidebarGroup } from 'starlight-openapi';

// GitHub Pages project site. If the repo moves org, or the site later gets a
// custom domain, these two constants are the only thing that has to change.
const SITE = 'https://accreleus.github.io';
const BASE = '/quasar';

// The control-plane API reference is generated from the frozen contract itself
// (`protocol/openapi.yaml`), not transcribed. That makes the spec the single
// source of truth and means the reference cannot drift from the server, which
// a hand-written page inevitably would. `protocol/` is a submodule, so this is
// the one part of the site that needs `git submodule update --init protocol`
// before it will build.
const apiSidebarGroup = createOpenAPISidebarGroup();

// Building the site now reads the protocol submodule (previously only the Go
// drift test did), so a fresh clone without `--recurse-submodules` would fail
// inside the OpenAPI parser with a bare ENOENT. Say what is actually wrong.
const API_SCHEMA = '../protocol/openapi.yaml';
if (!existsSync(fileURLToPath(new URL(API_SCHEMA, import.meta.url)))) {
	throw new Error(
		`Cannot find ${API_SCHEMA}, which the generated API reference is built from.\n` +
			'The protocol contracts are a submodule. From the repository root:\n\n' +
			'  git submodule update --init protocol\n',
	);
}

export default defineConfig({
	site: SITE,
	base: BASE,
	trailingSlash: 'always',
	integrations: [
		starlight({
			plugins: [
				starlightOpenAPI([
					{
						base: 'developer/api',
						schema: API_SCHEMA,
						sidebar: {
							group: apiSidebarGroup,
							label: 'Control-plane API',
							collapsed: true,
							operations: { badges: true, labels: 'path', sort: 'alphabetical' },
							tags: { sort: 'alphabetical' },
						},
						// The three real client surfaces: a terminal, the web client,
						// and photon. No Java or C# consumer exists, so offering one
						// would only imply support that is not there.
						snippets: {
							operation: {
								clients: {
									shell: ['curl'],
									javascript: ['fetch'],
									rust: ['reqwest'],
								},
								default: { target: 'shell', client: 'curl' },
							},
						},
					},
				]),
			],
			title: 'Quasar',
			description:
				'Self-hostable cloud gaming. Run games on your own GPU servers and play them in a browser.',
			logo: {
				src: './src/assets/quasar-mark.svg',
				alt: 'Quasar',
			},
			customCss: ['./src/styles/theme.css'],
			// Corrects the titles starlight-openapi generates; see the file.
			routeMiddleware: './src/route-data.ts',
			head: [
				{
					// Quasar is a dark-first product, so the docs open dark rather than
					// following the operating system. The toggle still works and a
					// stored choice always wins; this only sets the first-visit default.
					tag: 'script',
					content:
						"try{if(!localStorage.getItem('starlight-theme')){localStorage.setItem('starlight-theme','dark');document.documentElement.dataset.theme='dark'}}catch(e){}",
				},
			],
			social: [
				{
					icon: 'github',
					label: 'GitHub',
					href: 'https://github.com/accreleus/quasar',
				},
			],
			editLink: {
				baseUrl: 'https://github.com/accreleus/quasar/edit/main/site/',
			},
			lastUpdated: true,
			expressiveCode: {
				themes: ['github-dark-default', 'github-light'],
				styleOverrides: {
					borderRadius: '11px',
					codeFontFamily: "'JetBrains Mono', ui-monospace, monospace",
					codeFontSize: '0.85rem',
				},
			},
			sidebar: [
				{
					label: 'Start here',
					items: [
						{ label: 'What Quasar is', slug: 'start/what-quasar-is' },
						{ label: 'How it works', slug: 'start/how-it-works' },
						{ label: 'Requirements', slug: 'start/requirements' },
					],
				},
				{
					label: 'Install',
					items: [
						{ label: 'Quick start', slug: 'start/quickstart' },
						{ label: 'Install Quasar', slug: 'install/install' },
						{ label: 'First-run setup', slug: 'install/first-run' },
						{ label: 'Check your install', slug: 'install/verify' },
						{ label: 'Add a second GPU host', slug: 'install/second-host' },
					],
				},
				{
					label: 'Playing',
					items: [
						{ label: 'Your library', slug: 'playing/library' },
						{ label: 'In a session', slug: 'playing/in-session' },
						{ label: 'Browser support', slug: 'playing/browsers' },
						{ label: 'Files and saves', slug: 'playing/storage' },
					],
				},
				{
					label: 'Administration',
					items: [
						{ label: 'The admin area', slug: 'admin/overview' },
						{ label: 'Users and invites', slug: 'admin/users' },
						{ label: 'Images', slug: 'admin/images' },
						{ label: 'Apps and runtime presets', slug: 'admin/apps' },
						{ label: 'The Steam library', slug: 'admin/steam' },
						{ label: 'Quality profiles', slug: 'admin/profiles' },
						{ label: 'Hosts and GPUs', slug: 'admin/hosts' },
						{ label: 'Sessions and audit log', slug: 'admin/sessions' },
						{ label: 'Jobs and schedules', slug: 'admin/jobs' },
						{ label: 'Updating Quasar', slug: 'admin/releases' },
					],
				},
				{
					label: 'Networking and TLS',
					items: [
						{ label: 'HTTPS and certificates', slug: 'network/https' },
						{ label: 'Reaching Quasar remotely', slug: 'network/remote-access' },
						{ label: 'Behind a reverse proxy', slug: 'network/reverse-proxy' },
					],
				},
				{
					label: 'Tuning the stream',
					items: [
						{ label: 'Encoders', slug: 'tuning/encoders' },
						{ label: 'Codecs', slug: 'tuning/codecs' },
						{ label: 'Adaptive bitrate', slug: 'tuning/adaptive-bitrate' },
						{ label: 'Latency and smoothness', slug: 'tuning/latency' },
						{ label: 'Audio and microphone', slug: 'tuning/audio' },
						{ label: 'Experimental options', slug: 'tuning/experimental' },
					],
				},
				{
					label: 'Running it',
					items: [
						{ label: 'Health and logs', slug: 'operations/health' },
						{ label: 'Upgrading', slug: 'operations/upgrading' },
						{ label: 'Backup and restore', slug: 'operations/backup' },
						{ label: 'Uninstalling', slug: 'operations/uninstall' },
					],
				},
				{
					label: 'Troubleshooting',
					items: [
						{ label: 'Install and startup', slug: 'troubleshooting/install' },
						{ label: 'Connecting and certificates', slug: 'troubleshooting/connecting' },
						{ label: 'Launching a game', slug: 'troubleshooting/launching' },
						{ label: 'Picture, sound and input', slug: 'troubleshooting/av' },
						{ label: 'Stream quality', slug: 'troubleshooting/quality' },
						{ label: 'Diagnostics', slug: 'operations/diagnostics' },
					],
				},
				{
					label: 'Reference',
					items: [
						{ label: 'Environment variables', slug: 'reference/environment' },
						{ label: 'Ports and endpoints', slug: 'reference/ports' },
						{ label: 'Glossary', slug: 'reference/glossary' },
					],
				},
				{
					label: 'Developing Quasar',
					items: [
						{ label: 'Developer overview', slug: 'developer/overview' },
						{ label: 'Set up a checkout', slug: 'developer/getting-started' },
						{ label: 'Architecture and contracts', slug: 'developer/architecture' },
						{ label: 'Testing and verification', slug: 'developer/verification' },
						{ label: 'GPU and remote workflows', slug: 'developer/gpu-workflows' },
					],
				},
				apiSidebarGroup,
			],
		}),
	],
});
