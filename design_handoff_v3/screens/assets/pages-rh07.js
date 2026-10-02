// RH-07 surfaces (#394): the quick start's engine choice, host preparation and
// install output, console mode on an owned host, and the readiness card's
// engine facts and "skipped, and why" rows. Renders into rh07-v3.html.
//
// Two styles, because the surfaces live in two places:
//  - qs*()        the documentation site's quick start, in the site's own
//                 Starlight + qs-* style (site/src/components/QuickStart.astro).
//  - pageR7*()    admin console surfaces, in console-v3 classes and the ui.js
//                 helpers, reusing the approved RH-06 helpers from
//                 pages-rh06.js (snippet, diag, stage, modal, railFact, dis,
//                 muted, rhDetailHead, rhHost), which rh07-v3.html loads first.
//
// Names, versions, digests and paths are fictional placeholders.

/* ===========================================================================
   A. THE QUICK START (site style)
   ======================================================================== */

const QS_STEPS=['Host','Engine','Role','Storage','Owner','Access','Result'];
const QS_PLATFORM={fedora:'Fedora',ubuntu:'Ubuntu 24.04',debian:'Debian',arch:'Arch',unraid:'Unraid',other:'Another systemd Linux'};
const QS_NS='ghcr.io/accreleus/quasar';
const qsImg=n=>`${QS_NS}/${n}@sha256:<digest>`;

// The four engine profiles a machine can choose, in a neutral order: there is
// no default engine yet (D3).
const QS_ENGINES=[
 {id:'docker-rootful',label:'Docker, rootful',em:'Docker running as root, as Quasar runs today.'},
 {id:'docker-rootless',label:'Docker, rootless',em:'Docker installed for a dedicated quasar account.'},
 {id:'podman-rootless',label:'Podman, rootless',em:'Podman run by a dedicated quasar account.'},
 {id:'podman-rootful',label:'Podman, rootful',em:'Podman running as root.'}];
const qsEngine_=id=>QS_ENGINES.find(e=>e.id===id);

// What the site says about a platform + engine profile (D5). The docs'
// engine-profile page is the source; this mirrors it.
function qsProfile(platform,id){
 if(platform==='fedora')return id==='podman-rootful'
  ?{s:'experimental',why:'It has not been through the same tests as the other three yet. It becomes supported once it passes.'}
  :{s:'supported'};
 if(platform==='ubuntu')return {s:'experimental',why:'Nobody has run Quasar end to end on Ubuntu 24.04 yet. It should work.'};
 if(platform==='unraid')return id==='docker-rootful'?{s:'supported'}
  :{s:'unsupported',why:'Unraid has neither Podman nor rootless Docker.',alt:'Choose <strong>Docker, rootful</strong>: Unraid’s own Docker.'};
 return {s:'unsupported',why:`No engine profile has been tested on ${QS_PLATFORM[platform]}.`,alt:'Fedora is tested with Docker and Podman; Ubuntu 24.04 is experimental.'};
}
const QS_BADGE={supported:['Supported','success'],experimental:['Experimental','caution'],unsupported:['Unsupported','danger']};
const qsBadge=s=>`<span class="sl-badge small ${QS_BADGE[s][1]}">${QS_BADGE[s][0]}</span>`;

// The wizard card: progress, one step, and the Back / Next bar.
function qsFrame(step,body,next=true){
 qsGroup++;
 return `<div class="site"><div class="sl-content"><div class="qs">
  <ol class="qs-progress" aria-label="Progress">${QS_STEPS.map((s,i)=>`<li class="${i<step?'on':''}">${s}</li>`).join('')}</ol>
  <section class="qs-step">${body}</section>
  <nav class="qs-nav"><button type="button"${step===1?' disabled':''}>Back</button><span class="qs-count">${step} of ${QS_STEPS.length}</span><button type="button" data-next${next&&step<QS_STEPS.length?'':' disabled'}>Next</button></nav>
 </div></div></div>`;
}
// Each specimen gets its own radio group name: radios sharing a name across the
// gallery would uncheck each other.
let qsGroup=0;const qsName=n=>`${n}-${qsGroup}`;
const qsRadio=(name,checked,strong,em,extra)=>`<label><input type="radio" name="${qsName(name)}"${checked?' checked':''} /><span><strong>${strong}</strong>${em?`<em>${em}</em>`:''}</span>${extra||''}</label>`;
// full=true lifts the site's 26rem scroll cap, so the whole text is in the screenshot.
const qsCode=(t,full)=>`<pre${full?' style="max-height:none"':''}><code>${esc(t)}</code></pre>`;
const qsOutHead=(t,h='h3')=>`<div class="qs-out-head"><${h}>${t}</${h}><button type="button">Copy</button></div>`;

/* Step 1, amended: the platform list names Fedora's image-based editions and
   splits "Debian or Ubuntu", because only Ubuntu 24.04 has a profile. */
function qsHost(){
 return qsFrame(1,`<h2>Where are you deploying Quasar?</h2>
  <p class="qs-hint">This sets sensible defaults later, and decides which container engines are supported here.</p>
  <div class="qs-choices">
   ${qsRadio('platform',true,'Fedora','What the install is verified on. Includes Silverblue, Bazzite and uCore')}
   ${qsRadio('platform',false,'Ubuntu 24.04','Experimental')}
   ${qsRadio('platform',false,'Debian')}
   ${qsRadio('platform',false,'Arch')}
   ${qsRadio('platform',false,'Unraid','Different paths, different persistence')}
   ${qsRadio('platform',false,'Another systemd Linux')}
  </div>
  <p class="qs-note">Fedora is what the install is verified on. SELinux can stay enforcing.</p>`);
}

/* Step 2, new: the container engine and its mode. The page cannot see the
   machine, so it asks; the one-liner says what is installed, and the install
   checks the engine again on the machine and stops on a mismatch. */
function qsEngine(platform,chosen){
 const opts=QS_ENGINES.map(e=>{
  const p=qsProfile(platform,e.id);
  return qsRadio('engine',e.id===chosen,e.label,e.em,`<span style="margin-left:auto">${qsBadge(p.s)}</span>`);
 }).join('');
 let note,ok=true;
 if(!chosen){ok=false;
  note=`<p class="qs-note">Rootless is the safer choice: if Quasar were ever compromised, it could not take over the machine. On a rootful engine, whoever controls the engine’s socket controls the machine, and Quasar holds that socket. <a>Engine profiles</a> lists what each choice is tested on.</p>`;}
 else{
  const e=qsEngine_(chosen),p=qsProfile(platform,chosen),name=`${QS_PLATFORM[platform]} with ${e.label.replace(',','')}`;
  if(p.s==='supported')note=`<p class="qs-note">${qsBadge('supported')} <strong>${name}</strong> is tested with AMD and NVIDIA GPUs. Intel GPUs are not supported yet.${/rootful/.test(chosen)?' The engine runs as root, so its socket is equivalent to root on this machine; a rootless engine avoids that.':''} <a>Engine profiles</a></p>`;
  else if(p.s==='experimental')note=`<p class="qs-note qs-warn">${qsBadge('experimental')} <strong>${name}</strong> is experimental. ${p.why} You can install it; if something does not work, the host’s readiness card names it, and a report helps make this profile supported. <a>Engine profiles</a></p>`;
  else{ok=false;note=`<p class="qs-note qs-warn">${qsBadge('unsupported')} <strong>${name}</strong> is not supported. ${p.why} ${p.alt}</p>`;}
 }
 return qsFrame(2,`<h2>Which container engine will run Quasar?</h2>
  <p class="qs-hint">Quasar runs on Docker or Podman, either as root (rootful) or as an ordinary account (rootless). Not sure what this machine has? Run this on it:</p>
  ${qsCode('for e in docker podman; do command -v $e >/dev/null && $e --version; done')}
  <div class="qs-choices">${opts}</div>
  ${note}`,ok);
}

/* ---- the Result step ---------------------------------------------------- */

const QS_HOMES='/var/lib/quasar/homes';
function qsSummary(profile){
 const e=qsEngine_(profile),rootless=/rootless/.test(profile);
 const row=(k,v)=>`<div class="qs-sum-row"><dt>${k}</dt><dd>${v}</dd></div>`;
 return `<dl class="qs-summary">
  ${row('Host','Fedora')}
  ${row('Engine',`${e.label} &nbsp;${qsBadge('supported')}`)}
  ${row('Role','Combined host')}
  ${row('User homes',QS_HOMES)}
  ${row('Owned by',rootless?'the quasar account':'uid 1000, gid 1000')}
  ${row('Console',profile==='docker-rootless'?'https://living-room-pc.lan':'https://living-room-pc.lan:8443')}
  ${row('Database','Quasar’s own')}
 </dl>`;
}

function qsPrepCommand(profile){
 const [engine,mode]=profile.split('-');
 const flags=[`--mode ${mode}`,`--engine ${engine}`];
 if(mode==='rootless')flags.push(`--homes ${QS_HOMES}`);
 if(profile==='podman-rootless')flags.push('--console');
 if(profile==='docker-rootless')flags.push('--unprivileged-port-start 443');
 return `curl -fsSLO https://accreleus.github.io/quasar/prepare-host.sh\nsudo sh prepare-host.sh ${flags.join(' \\\n  ')}`;
}

// Step 1 of the Result: the one root step, its toggles, and what it changes.
function qsPrepBlock(profile,o={}){
 const console_=profile==='podman-rootless',lowPorts=profile==='docker-rootless';
 const chk=(on,t)=>`<label class="qs-check"><input type="checkbox"${on?' checked':''} /><span>${t}</span></label>`;
 return `<div class="qs-out">
  ${qsOutHead('1. Prepare the machine, once, as root')}
  <p class="qs-hint">The only step that needs root; Quasar itself never runs as root. It ${/rootless/.test(profile)?'creates the quasar account, gives it':'creates a quasar group, gives it'} the devices Quasar uses and nothing more, and sets the kernel settings Quasar needs. It prints every change and why, and is safe to run again.</p>
  ${chk(console_,'This machine also shows games on its own screen (console mode). Adds its display, sound and monitor control.')}
  ${chk(false,'Let Quasar read GPU fault messages from the kernel log. Optional; every account on the machine can then read it.')}
  ${chk(lowPorts,lowPorts?'Let Quasar use port 443. Needed because you chose a port below 1024.':'Let Quasar use ports below 1024. Only needed if you choose one under “Change the ports”.')}
  ${qsCode(qsPrepCommand(profile))}
  <details class="qs-adv"${o.open?' open':''}><summary>What it changes</summary>${o.open?o.body:''}</details>
 </div>`;
}

// The printed lines, from deploy/prepare-host.sh's own strings.
const QS_PREP_FIRST=`Quasar host preparation (rootless)
  note     engine: podman
  changed  account quasar — a rootless install runs everything under this one unprivileged account
  changed  /etc/subuid — subordinate uid range for quasar: the containers' own users map into it
  changed  /etc/subgid — subordinate gid range for quasar: the containers' own users map into it
  changed  lingering for quasar — its engine and Quasar keep running with nobody logged in, and start at boot
  changed  /etc/udev/rules.d/70-quasar.rules — give the quasar group the devices Quasar uses, and only those
  changed  /etc/modules-load.d/quasar.conf — load uinput i2c-dev at boot
  ok       kernel module uinput loaded
  changed  /etc/sysctl.d/99-quasar.conf — kernel settings Quasar needs, applied at every boot
  changed  net.core.wmem_default=2097152 now — it was 212992
  note     GPU fault messages stay hidden from Quasar; --allow-kernel-log enables that optional diagnostic
  changed  /etc/cdi/nvidia.yaml — describes the NVIDIA GPU (driver 580.95.05) to the engine, so containers get it without extra privilege
  changed  SELinux container_use_xserver_devices on — lets confined containers open the NVIDIA device nodes (labelled xserver_misc_device_t), and nothing else; SELinux stays enforcing
  changed  podman-restart.service enabled for quasar — Podman has no daemon, so this is what starts Quasar's containers at boot
  changed  podman.socket enabled for quasar — the engine socket the Quasar seed and recovery actor talk to
  changed  homes root ${QS_HOMES} — where each user's game saves and settings live
Host preparation is complete. Run it again at any time: it changes only what is missing.`;
const QS_PREP_RERUN=`Quasar host preparation (rootless)
  note     engine: podman
  ok       account quasar
  ok       /etc/subuid entry for quasar
  ok       /etc/subgid entry for quasar
  ok       lingering for quasar
  ok       /etc/udev/rules.d/70-quasar.rules
  ok       /etc/modules-load.d/quasar.conf
  ok       kernel module uinput loaded
  ok       kernel module i2c-dev loaded
  ok       /etc/sysctl.d/99-quasar.conf
  ok       net.core.wmem_default=2097152 (running kernel)
  note     GPU fault messages stay hidden from Quasar; --allow-kernel-log enables that optional diagnostic
  ok       /etc/cdi/nvidia.yaml
  ok       SELinux container_use_xserver_devices on
  ok       podman-restart.service enabled for quasar
  ok       podman.socket enabled for quasar
  ok       homes root ${QS_HOMES}
Host preparation is complete. Run it again at any time: it changes only what is missing.`;

function qsPrepOutput(which){
 const body=`<p class="qs-hint" style="margin-top:.75rem">${which==='first'?'On a first run, on this Fedora machine with an NVIDIA GPU:':'Run again, with nothing left to change:'}</p>
  <pre style="white-space:pre-wrap;max-height:none"><code>${esc(which==='first'?QS_PREP_FIRST:QS_PREP_RERUN)}</code></pre>
  <p class="qs-hint">Every file it writes is under <code>/etc</code>, plus the quasar account’s own home and systemd’s lingering record. It never writes under <code>/usr</code>, never re-owns existing files and never relaxes SELinux.</p>`;
 return qsFrame(7,`<h2>Your install</h2><p class="qs-hint">The summary and step 2 are as in the specimen above.</p>${qsPrepBlock('podman-rootless',{open:true,body})}`);
}

function qsSeedEnv(){
 return [['QUASAR_ROLE','combined'],['QUASAR_PUBLIC_HOST','living-room-pc.lan'],['QUASAR_HOME_ROOT',QS_HOMES],['QUASAR_TEMPLATE_ROOT','/var/lib/quasar/templates'],
  ['QUASAR_CONTROL_PLANE_IMAGE',qsImg('quasar-control-plane')],['QUASAR_AGENT_IMAGE',qsImg('quasar-node-agent')]];
}

// Podman: the seed as a Quadlet unit (D15). The socket mount's target path is
// illustrative until the ADR 0007 amendment (D22) fixes it.
function qsQuadlet(){
 return `# Quasar's seed: the one container this machine declares.
# Generated by the quick start. Read it before you install it.
[Unit]
Description=Quasar seed
Wants=network-online.target
After=network-online.target

[Container]
ContainerName=quasar-seed
Image=${qsImg('quasar-recovery')}
Exec=seed
SecurityLabelDisable=true
Volume=%t/podman/podman.sock:/run/podman/podman.sock
Volume=quasar-machine:/var/lib/quasar-machine:ro
${qsSeedEnv().map(([k,v])=>`Environment=${k}=${v}`).join('\n')}

[Service]
Restart=always

[Install]
WantedBy=default.target`;
}
const QS_PODMAN_PINS=`for i in quasar-recovery quasar-control-plane quasar-node-agent; do
  podman pull -q ${QS_NS}/$i:o2-develop >/dev/null &&
  podman image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' \\
    ${QS_NS}/$i:o2-develop | grep -m1 "^${QS_NS}/$i@"
done`;
const QS_PODMAN_RUN=`podman run -d --name quasar-seed --security-opt label=disable \\
  -v "$XDG_RUNTIME_DIR/podman/podman.sock:/run/podman/podman.sock" \\
  -v quasar-machine:/var/lib/quasar-machine:ro \\
${qsSeedEnv().map(([k,v])=>`  -e ${k}='${v}' \\`).join('\n')}
  ${qsImg('quasar-recovery')} seed`;

// Docker: today's script, minus the host preparation it used to do itself,
// plus an engine check. Abridged where it is unchanged.
function qsDockerScript(rootless){
 const mode=rootless?'rootless':'rootful';
 return `#!/usr/bin/env bash
# Quasar quick start: a combined host on Fedora, Docker ${mode}. Generated in
# your browser; nothing was sent anywhere. Read it before you run it.
set -euo pipefail
${rootless?'export DOCKER_HOST="unix://$XDG_RUNTIME_DIR/docker.sock"\n':''}
echo "==> Engine"
# Made for Docker, ${mode}. Stop if this machine runs something else.
mode=rootful
docker info --format '{{.SecurityOptions}}' | grep -q rootless && mode=rootless
if [ "$mode" != ${mode} ]; then
  echo "This machine's Docker is $mode. Choose Docker, $mode in the quick start." >&2
  exit 1
fi

echo "==> Host preflight"            # … unchanged
echo "==> Existing installs"         # … unchanged
echo "==> Images"                    # … unchanged: pins each image to its digest

echo "==> Starting the seed"
docker run -d --name quasar-seed --restart unless-stopped \\
  --security-opt label=disable \\
  -v ${rootless?'"$XDG_RUNTIME_DIR/docker.sock"':'/var/run/docker.sock'}:/var/run/docker.sock \\
  -v quasar-machine:/var/lib/quasar-machine:ro \\
  -e QUASAR_ROLE='combined' \\
  -e QUASAR_PUBLIC_HOST='living-room-pc.lan' \\${rootless?"\n  -e QUASAR_TLS_PORT='443' \\":''}
  …
  "$seed_image" seed >/dev/null

echo "==> Waiting for Quasar"         # … unchanged`;
}

function qsResult(profile){
 let install;
 if(profile==='podman-rootless')install=`<div class="qs-out">
  ${qsOutHead('2. Install Quasar as the quasar account')}
  <p class="qs-hint">Podman starts Quasar from a Quadlet unit, the way systemd starts any service, and again at every boot. As the quasar account (<code>sudo machinectl shell quasar@</code>), print the image pins, then replace each <code>&lt;digest&gt;</code> below with them:</p>
  ${qsCode(QS_PODMAN_PINS)}
  <p class="qs-hint">Save this as <code>~quasar/.config/containers/systemd/quasar-seed.container</code>:</p>
  ${qsCode(qsQuadlet(),true)}
  ${qsOutHead('Then start it, as the quasar account','h4')}
  ${qsCode('systemctl --user daemon-reload\nsystemctl --user start quasar-seed')}
  <details class="qs-adv"><summary>Only trying it out? Run the seed with podman run instead</summary></details>
  <p class="qs-note">The seed creates Quasar’s recovery actor, which generates every secret and creates the rest. Nothing here writes a Compose file or an .env.</p>
 </div>`;
 else if(profile==='docker-rootful')install=`<div class="qs-out">
  ${qsOutHead('2. Run this on the machine')}
  <p class="qs-hint">Read it before you run it. It checks the engine and the host, pins the images to their digests, and starts one container, the seed. Quasar generates its own secrets on the machine. Shortened here where it is unchanged.</p>
  ${qsCode(qsDockerScript(false),true)}
 </div>
 <p class="qs-hint">“Using Dockge or Arcane? Paste this stack instead” follows, unchanged.</p>`;
 else install=`<div class="qs-out">
  ${qsOutHead('2. Set up Docker for the quasar account')}
  <p class="qs-hint">Rootless Docker belongs to the quasar account. Host preparation turned lingering on, so it keeps running with nobody logged in.</p>
  ${qsCode("sudo machinectl shell quasar@ /bin/sh -c \\\n  'dockerd-rootless-setuptool.sh install && systemctl --user enable --now docker'")}
 </div>
 <div class="qs-out">
  ${qsOutHead('3. Run this as the quasar account')}
  <p class="qs-hint">The same script as for rootful Docker, talking to the quasar account’s own Docker. Shortened here where it is unchanged.</p>
  ${qsCode(qsDockerScript(true),true)}
 </div>`;
 return qsFrame(7,`<h2>Your install</h2>${qsSummary(profile)}${qsPrepBlock(profile)}${install}
  <p class="qs-hint">Then open Quasar and continue with <a>first-run setup</a>.</p>`);
}

/* ===========================================================================
   B. ADMIN CONSOLE SURFACES (console-v3)
   ======================================================================== */

// The owned host these surfaces are drawn on: the RH-06 story's combined host,
// now on Podman rootless, with its TV attached for console mode.
const R7H={...rhHost('living-room-pc'),engine:'Podman',engineVer:'5.6.2',mode:'rootless'};
const r7Crumbs=(h,leaf)=>`<a>Fleet</a>${icon('chev')}<a>${h.name}</a>${icon('chev')}<span>${leaf}</span>`;

/* ---- D. console mode on an owned host ----------------------------------- */

// states: off | applying | on | alsa | failed | unprepared
function pageR7Console(state){
 const h=R7H;
 const lock=state==='applying';
 const on=['on','alsa','applying'].includes(state);
 const alsa=state==='alsa';
 const row=(t,p,ctrl)=>`<div class="cset"><div><h3>${t}</h3><p class="hint">${p}</p></div><div>${ctrl}</div></div>`;
 const sw=(v,d)=>`<span class="switch" role="switch" aria-checked="${v}"${d?' style="opacity:.45;cursor:not-allowed"':''}></span>`;
 const sel=(opts,w)=>`<select class="select" style="width:${w||'260px'}${lock?';opacity:.5;cursor:not-allowed" disabled':'"'}>${opts.map(o=>`<option>${o}</option>`).join('')}</select>`;
 const grp=t=>`<div class="eyebrow" style="padding:var(--s5) var(--card-pad) 2px">${t}</div>`;
 const tryAgain=`<button class="btn btn-primary">${icon('refresh')}Try again</button>`;
 const note={
  off:`<div class="note" style="margin-bottom:var(--s4)"><strong>Console mode is off.</strong> Turned on, ${h.name} shows games on its own screen. Its recovery actor replaces the node agent with one that can use this machine’s display, sound and monitor control, which ends the host’s 1 live session.</div>`,
  applying:`<div class="note" style="margin-bottom:var(--s4)"><strong>Turning on console mode.</strong> The recovery actor is replacing the node agent (started 14:12). Console mode reads as on only once the new node agent is healthy; if it is not, the previous one is put back. New sessions on this host wait until then.</div>`,
  failed:`<div class="note warn" style="margin-bottom:var(--s4);display:flex;gap:var(--s5);align-items:center;flex-wrap:wrap"><div style="flex:1;min-width:280px"><strong>Console mode did not turn on.</strong> The new node agent could not take the display: another program holds it (gdm, the login screen). The recovery actor put the previous node agent back, so console mode is off and streaming works as before. Stop the login screen on this display, or choose another output, then try again.${diag('Details','attempt: 7c21…d0 · console mode on · outcome: failed, restored\nreason: display held by another process (gdm, pid 1184)\nreadiness check: console_display (fail)\nnode agent: restored to the previous recipe')}</div>${tryAgain}</div>`,
  unprepared:`<div class="note warn" style="margin-bottom:var(--s4)"><div style="display:flex;gap:var(--s5);align-items:center;flex-wrap:wrap"><div style="flex:1;min-width:280px"><strong>Console mode did not turn on: ${h.name} is not prepared for it.</strong> The new node agent could not open this machine’s display, sound or monitor control, because host preparation ran without console mode. The recovery actor put the previous node agent back; streaming works as before. Run host preparation again with console mode, then try again. It adds those devices and changes nothing else.</div>${tryAgain}</div>
   <div style="margin-top:var(--s4)">${snippet(`Run on ${h.name} as root`,`sudo sh prepare-host.sh --mode rootless --engine podman --homes /var/lib/quasar/homes --console`)}</div>
   ${diag('Details','attempt: 3f08…a2 · console mode on · outcome: failed, restored\nreason: no access to /dev/dri/card1, /dev/snd, /dev/i2c-4\nreadiness checks: console_display, console_audio, console_ddc (fail)')}</div>`}[state]||'';
 const stateChip=state==='applying'?chip('applying','info'):on?chip('on','success'):chip('off');
 const audioOpts=alsa?['HDMI / DisplayPort (RTX 4080 Super)','Analog line-out','Quiet (no local audio)']:['Host PipeWire · default output','Host PipeWire · HDMI (LG TV)','Quiet (no local audio)'];
 const audioHelp=alsa?'No PipeWire runs on this machine, so console audio goes straight to the sound device (ALSA).'
  :'This machine runs PipeWire, so console audio plays through it, beside the desktop’s own sound. Quasar never takes the sound device from it.';
 return `<div class="page">
 ${head('Local console',`Local display on ${h.name}`,
  `<button class="btn btn-ghost"${lock?' '+dis:''}>Discard</button><button class="btn btn-primary"${lock||state==='failed'||state==='unprepared'?' '+dis:''}>Save changes</button>`,
  r7Crumbs(h,'Local console'))}
 ${note}
 <div class="split" style="grid-template-columns:1fr 300px">
  <div class="card">
   <div class="panel-head"><div><span class="panel-title">Console mode</span><div class="hint" style="margin-top:3px">This machine shows games on its own screen, and can stream them too.</div></div>
    <div class="acts">${stateChip}${sw(on,lock)}</div></div>
   ${grp('Display')}
   ${row('Physical output','The screen console mode uses. Automatic picks the connected one.',sel(['card1-HDMI-A-1 · LG TV','Automatic','card1-DP-1 · not connected']))}
   ${row('Physical mode','Resolution and refresh rate on that screen.',sel(['3840×2160 @ 60 Hz · preferred','2560×1440 @ 120 Hz','1920×1080 @ 60 Hz']))}
   ${grp('Streaming')}
   ${row('Also stream','Adds a browser stream of the same picture. Off is local only.',sw(true,lock))}
   ${grp('Local audio')}
   ${row('Local audio output',audioHelp,sel(audioOpts))}
   <div class="hint" style="padding:var(--s4) var(--card-pad);border-top:1px solid var(--line)">Input and Startup follow, unchanged from admin-console-v3.</div>
  </div>
  <div style="display:flex;flex-direction:column;gap:var(--s4)">
   <div class="card card-pad"><div class="eyebrow">Host</div><h3 style="font-size:var(--t-h3);margin-top:6px">${h.name}</h3><div class="mono" style="color:var(--text-3);font-size:var(--t-xs);margin-top:3px">${h.id}</div>
    <div style="margin-top:var(--s4)">${railFact('Engine',`${h.engine} ${h.engineVer}`)}${railFact('Mode',h.mode)}${railFact('Live sessions',['off','on','alsa'].includes(state)?'1':'0')}</div></div>
   <div class="card card-pad"><div class="eyebrow">Reported capabilities</div>
    <div style="display:flex;flex-direction:column;gap:9px;margin-top:10px;font-size:var(--t-xs);color:var(--text-3)">
     <div>Connectors: <span class="mono">HDMI-A-1, DP-1</span></div>
     <div style="border-left:2px solid var(--line-2);padding-left:9px;display:flex;flex-direction:column;gap:2px">
      <span class="cell-id" style="align-self:flex-start">card1-HDMI-A-1</span><span>LG TV · connected</span><span>preferred 3840×2160 @ 60.000 Hz</span></div>
     <div>Local audio: ${alsa?'ALSA · HDMI / DisplayPort, Analog line-out':'host PipeWire 1.4 · 2 outputs'}</div>
     <div>Monitor control: ${state==='unprepared'?'no access':'DDC on i2c-4'}</div></div></div>
  </div></div></div>`;
}

function r7ConsoleConfirm(){
 const p=t=>`<p style="margin:0 0 12px;font-size:var(--t-sm);color:var(--text-2);line-height:1.6">${t}</p>`;
 return modal(`Turn on console mode on ${R7H.name}?`,
  p(`Its recovery actor replaces the node agent with one that can use this machine’s display, sound and monitor control.`)+
  p(`The <b style="color:var(--text)">1 live session</b> on this host ends. If the new node agent does not become healthy, the previous one is put back and console mode stays off.`),
  `<button class="btn btn-ghost">Cancel</button><button class="btn btn-primary">Turn on console mode</button>`);
}

/* ---- E. the readiness card: engine facts and "skipped, and why" --------- */

// One check, drawn as ReadinessCard.tsx draws it in its grid layout.
const X_ICON=`<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M4 4l8 8M12 4l-8 8" stroke-linecap="round"/></svg>`;
const R7_GLYPH={pass:['rdy-ok','Pass',icon('check')],fail:['rdy-bad','Fail',X_ICON],warn:['rdy-warn','Warning',icon('alert')],skip:['rdy-off','Skipped','<span aria-hidden="true">–</span>']};
function r7Check([id,status,summary,o={}]){
 const [cls,label,glyph]=R7_GLYPH[status];
 const blocks=o.blocks?`<span title="Blocks every launch on this host">${chip(status==='fail'?'Blocks launches':'Can block launches',status==='fail'?'danger':'')}</span>`:'';
 return `<div class="readiness-check"><div class="host-setting-copy">
  <div class="rowflex" style="gap:var(--s2)"><span class="rdy-glyph ${cls}" role="img" aria-label="${label}" title="${label}">${glyph}</span><h3 class="rdy-title">${id.replaceAll('_',' ')}</h3>${blocks}</div>
  <p>${summary}</p>
  ${o.prov?`<p class="rdy-provenance">${o.prov}</p>`:''}
  ${o.fix?snippet('',o.fix):''}
 </div></div>`;
}

// Per host: engine facts (amendment 17's three register fields) and checks,
// grouped as web/src/lib/readiness/groups.ts groups them.
const PREP_FIX='sudo sh prepare-host.sh --mode rootless --engine podman --homes /var/lib/quasar/homes';
const XID_SKIP=['xid_visibility','skip','GPU fault messages are not collected, because this host keeps the kernel log restricted. This diagnostic is optional; host preparation can allow it (--allow-kernel-log).'];
const CONSOLE_SKIPS=[['console_display','skip','Console mode is off for this host.'],['console_audio','skip','Console mode is off for this host.'],['console_ddc','skip','Console mode is off for this host.']];
const R7_READY={
 rootless:{host:'living-room-pc',facts:['Podman','5.6.2','rootless'],groups:[
  ['Container runtime',[
   ['runtime_endpoint','pass','Podman’s API answers at /run/user/1100/podman/podman.sock.',{prov:'container runtime'}],
   ['runtime_engine','pass','Podman 5.6.2, rootless: a supported engine profile.',{prov:'container runtime'}],
   ['runtime_cdi','pass','The NVIDIA GPU reaches containers through CDI.',{prov:'container runtime',blocks:1}],
   ['engine_restart_on_boot','pass','Podman starts Quasar’s containers again at boot: its restart service is enabled for quasar, and lingering is on.',{prov:'local check'}],
   ['engine_healthchecks','pass','Podman can run health checks: quasar has a systemd user session.',{prov:'local check'}]]],
  ['GPU & display',[['media_probe','pass','Encoded a test stream on the GeForce RTX 4080 Super.',{prov:'Observed 28 Sep 2026, 14:18 · host probe',blocks:1}]]],
  ['Input & sandbox',[
   ['uinput','pass','The node agent can create virtual input devices.',{prov:'local check'}],
   ['input_device_access','pass','The quasar account can open the input devices Quasar creates, and no others.',{prov:'Observed 28 Sep 2026, 14:18 · host probe',blocks:1}]]],
  ['Storage',[['homes_root_writable','pass','/var/lib/quasar/homes is writable by quasar, and session files are owned by it.',{prov:'local check',blocks:1}]]],
  ['Network',[['media_reachability','pass','A browser on another machine reached this host during a session at 13:02.',{prov:'Observed 28 Sep 2026, 13:02 · container runtime'}]]]],
  skipped:[XID_SKIP,...CONSOLE_SKIPS]},
 rootful:{host:'gpu-host-4',facts:['Docker','28.4.0','rootful'],groups:[
  ['Container runtime',[
   ['runtime_endpoint','pass','Docker’s API answers at /var/run/docker.sock.',{prov:'container runtime'}],
   ['runtime_engine','pass','Docker 28.4.0, rootful: a supported engine profile. On a rootful engine its socket is equivalent to root on this machine.',{prov:'container runtime'}],
   ['runtime_cdi','pass','The NVIDIA GPU reaches containers through CDI.',{prov:'container runtime',blocks:1}]]],
  ['Input & sandbox',[
   ['uinput','pass','The node agent can create virtual input devices.',{prov:'local check'}],
   ['input_device_access','pass','The quasar group can open the input devices Quasar creates, and no others.',{prov:'Observed 28 Sep 2026, 14:02 · host probe',blocks:1}]]]],
  skipped:[
   ['engine_restart_on_boot','skip','Docker’s own daemon starts Quasar’s containers at boot, so there is nothing to check.'],
   ['engine_healthchecks','skip','Only Podman needs a systemd session for health checks; Docker runs them itself.'],
   XID_SKIP,...CONSOLE_SKIPS]},
 experimental:{host:'gpu-host-2',facts:['Docker','28.4.0','rootless'],groups:[
  ['Container runtime',[
   ['runtime_engine','warn','Docker 28.4.0, rootless, on Ubuntu 24.04: an experimental engine profile. It has not been run end to end yet, so a problem here may be one nobody has seen. See Engine profiles.',{prov:'container runtime'}],
   ['runtime_endpoint','pass','Docker’s API answers at /run/user/1100/docker.sock.',{prov:'container runtime'}],
   ['engine_restart_on_boot','pass','quasar’s Docker service is enabled, and lingering is on.',{prov:'local check'}]]],
  ['GPU & display',[['media_probe','pass','Encoded a test stream on the Radeon RX 7800 XT.',{prov:'Observed 28 Sep 2026, 14:11 · host probe',blocks:1}]]]],
  skipped:[['runtime_cdi','skip','No NVIDIA GPU on this host.'],['engine_healthchecks','skip','Only Podman needs a systemd session for health checks.'],XID_SKIP,...CONSOLE_SKIPS]},
 fail:{host:'gpu-host-6',facts:['Podman','5.6.2','rootless'],groups:[
  ['Input & sandbox',[
   ['input_device_access','fail','Quasar created a virtual gamepad, but the quasar account cannot open it: the host rule for Quasar’s input devices is missing. Sessions would start with no input.',{prov:'Observed 28 Sep 2026, 14:20 · host probe',blocks:1,fix:PREP_FIX}],
   ['uinput','pass','The node agent can create virtual input devices.',{prov:'local check'}]]],
  ['Container runtime',[
   ['engine_restart_on_boot','fail','Podman will not start Quasar’s containers again after a reboot: lingering is off for quasar.',{prov:'local check',fix:PREP_FIX}],
   ['runtime_engine','pass','Podman 5.6.2, rootless: a supported engine profile.',{prov:'container runtime'}]]]],
  skipped:[['runtime_cdi','skip','No NVIDIA GPU on this host.'],XID_SKIP,...CONSOLE_SKIPS]},
 unknown:{host:'gpu-host-3',facts:null,groups:[
  ['Container runtime',[['runtime_endpoint','pass','Docker’s API answers at /var/run/docker.sock.',{prov:'container runtime'}]]],
  ['Input & sandbox',[['uinput','pass','The node agent can create virtual input devices.',{prov:'local check'}]]]],
  skipped:[['xid_visibility','skip','This host has no NVIDIA driver loaded.']]}};

function r7ReadinessCard(v){
 const d=R7_READY[v];
 const anyFail=d.groups.some(([,cs])=>cs.some(c=>c[1]==='fail'));
 const facts=d.facts
  ?`${railFact('Engine',d.facts[0])}${railFact('Version',`<span class="num">${d.facts[1]}</span>`)}${railFact('Mode',d.facts[2])}`
  :`${railFact('Engine',muted('not reported'))}${railFact('Version',muted('—'))}${railFact('Mode',muted('—'))}`;
 return `<div class="card sec-card">
  <div class="sec-head"><div><h3>Readiness</h3><div class="desc">Last reported 28 Sep 2026, 14:20</div></div>${anyFail?`<div class="acts">${chip('Needs attention','danger')}</div>`:''}</div>
  <div class="readiness-group" style="margin-bottom:var(--s5)"><div class="eyebrow">Container engine</div>
   <div style="max-width:420px">${facts}</div>
   ${d.facts?'':`<p class="hint" style="margin:var(--s2) 0 0">This host’s node agent is older than engine reporting, so the engine reads as unknown until the agent is updated.</p>`}</div>
  <p class="hint" style="margin:0 0 var(--s5)">Readiness is what this host can establish about itself. It does not show whether a browser can reach the host; that depends on the network between them.</p>
  <div style="display:flex;flex-direction:column;gap:var(--s4)">
   ${d.groups.map(([g,cs])=>`<div class="readiness-group"><div class="eyebrow">${g}</div><div class="readiness-grid">${cs.map(r7Check).join('')}</div></div>`).join('')}
  </div>
  <details class="readiness-more" open><summary>${d.skipped.length} ${d.skipped.length===1?'check':'checks'} skipped, and why</summary>
   <div class="readiness-grid" style="margin-top:var(--s3)">${d.skipped.map(r7Check).join('')}</div></details>
 </div>`;
}
function pageR7Readiness(v){
 const h=rhHost(R7_READY[v].host)||RH_NEW;
 return `<div class="page">${rhDetailHead(h)}${r7ReadinessCard(v)}
  <div class="hint" style="margin-top:var(--s4)">The other host-page cards are unchanged.</div></div>`;
}
