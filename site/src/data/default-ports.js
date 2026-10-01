// Default listeners documented in docs/configuration.md and deploy seed inputs.
// UDP uses the GPU host's actual kernel ephemeral range, rather than a fixed port.
export const DEFAULT_PORTS = [
  ['8443', 'TCP / HTTPS', 'Web app and API', 'Browser → control plane'],
  ['8080', 'TCP / HTTP', 'Enrollment and health', 'GPU host → control plane; local health checks'],
  ['9091', 'TCP / HTTP', 'Agent health', 'Localhost on GPU host only'],
  ['Host ephemeral range', 'UDP', 'Game stream and input', 'Browser ↔ GPU host, directly'],
  ['5353', 'UDP / mDNS', 'Chromium local ICE candidates', 'LAN peers ↔ GPU host'],
];
