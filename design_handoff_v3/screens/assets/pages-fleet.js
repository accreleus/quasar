// Fleet: Hosts · Host detail · Storage · Images
const F=window.QDATA;
const FLEET_TABS=n=>tabs([
 {id:'hosts',label:'Hosts',count:F.HOSTS.length,go:'#/fleet/hosts'},
 {id:'storage',label:'Storage',count:F.STORAGE.length,go:'#/fleet/storage'}],n);
const HOST_OPEN=new Set();
function hostExp(id,ev){ev.stopPropagation();const r=document.getElementById('ex-'+id),b=document.getElementById('xb-'+id);const open=r.style.display==='none';r.style.display=open?'table-row':'none';b.setAttribute('aria-expanded',open);if(open)HOST_OPEN.add(id);else HOST_OPEN.delete(id);}
function pageHosts(){
 const online=F.HOSTS.filter(h=>h.state==='online').length;
 const row=h=>{
  const v=h.gpus.reduce((a,g)=>a+g.vram[0],0),vt=h.gpus.reduce((a,g)=>a+g.vram[1],0);
  const su=h.gpus.reduce((a,g)=>a+g.slots[0],0),st=h.gpus.reduce((a,g)=>a+g.slots[1],0);
  const sp=pct(su,st),vp=pct(v,vt),dp=pct(h.storeTotal-h.storeFree,h.storeTotal);
  const dim=h.state==='offline'?'opacity:.55':'';
  const op=HOST_OPEN.has(h.id);
  const u=(l,val,total,txt,p)=>`<div class="bar-row"><span>${l}</span>${bar(val,total,tone(p))}<span class="v">${txt}</span></div>`;
  return `<tr class="clickable" style="${dim}" onclick="go('#/fleet/hosts/${h.id}')">
   <td style="width:34px;padding-right:0"><button class="exp-btn" id="xb-${h.id}" aria-expanded="${op}" title="Show capacity and storage" onclick="hostExp('${h.id}',event)">${icon('chev')}</button></td>
   <td><div class="rowflex">${sdot(h.state)}<span class="primary">${h.name}</span></div>
       <div class="sub mono" style="margin-top:2px;padding-left:17px">${h.id}${h.state!=='online'?' · '+h.state:''}</div></td>
   <td><div class="stack"><span>${h.gpus[0].n}${h.gpus.length>1?` <span class="sub">×${h.gpus.length}</span>`:''}</span><span class="sub">${h.gpus[0].v}</span></div></td>
   <td style="min-width:230px"><div class="u2">
     ${u('GPU',su,st,`${su}/${st}`,sp)}${u('VRAM',v,vt,`${Math.round(v)}/${vt}`,vp)}
     ${u('RAM',h.ramPct,100,`${h.ramPct}%`,h.ramPct)}${u('DISK',h.storeTotal-h.storeFree,h.storeTotal,`${dp}%`,dp)}
    </div></td>
   <td class="right num">${h.sessions}</td>
   <td class="right num" style="color:${h.state==='online'?'var(--success-text)':h.state==='offline'?'var(--danger-text)':'var(--warning-text)'}">${h.hb.replace(' ago','')}</td>
   <td class="cell-actions">${menu([{label:'Open host',fn:`go('#/fleet/hosts/${h.id}')`},{label:'Local console',fn:`go('#/fleet/hosts/${h.id}/console')`},{label:'Host settings',fn:`go('#/fleet/hosts/${h.id}/settings')`},'-',{label:'Drain'},{label:'Remove host',danger:1}])}</td></tr>
  <tr class="exp-row" id="ex-${h.id}" style="display:${op?'table-row':'none'}"><td colspan="7"><div class="exp-in">
   <div><div class="eyebrow">Hardware</div>
    <div class="exp-fact"><span>CPU</span><span>${h.cpu}</span></div>
    <div class="exp-fact"><span>Memory</span><span>${h.ram} · ${h.ramPct}% used</span></div>
    <div class="exp-fact"><span>Uptime</span><span>${h.uptime}</span></div>
    <div class="exp-fact"><span>Agent</span><span class="mono">${h.agent}</span></div></div>
   <div><div class="eyebrow">GPUs and slots</div>
    ${h.gpus.map((g,i)=>`<div class="exp-fact"><span>${g.n} #${i}</span><span class="num">${g.slots[0]}/${g.slots[1]} slots · ${g.vram[0]}/${g.vram[1]} GB</span></div>`).join('')}
    <div class="exp-fact"><span>Total</span><span class="num">${su}/${st} slots · ${Math.round(v)}/${vt} GB</span></div></div>
   <div><div class="eyebrow">Storage</div>
    <div class="exp-fact"><span>Used</span><span class="num">${h.storeTotal-h.storeFree} / ${h.storeTotal} GB</span></div>
    <div class="exp-fact"><span>Free</span><span class="num"${dp>=90?' style="color:var(--danger-text)"':''}>${h.storeFree} GB</span></div>
    <div class="exp-fact"><span>Root</span><span class="mono">/var/lib/quasar</span></div>
    <div style="margin-top:9px"><a onclick="event.stopPropagation();go('#/fleet/storage')">Storage detail</a></div></div>
   <div><div class="eyebrow">Actions</div>
    <div style="display:flex;flex-direction:column;gap:7px;align-items:flex-start;margin-top:2px">
     <button class="btn btn-sm btn-ghost" onclick="event.stopPropagation();go('#/fleet/hosts/${h.id}')">Open host</button>
     <button class="btn btn-sm btn-ghost" onclick="event.stopPropagation();go('#/fleet/hosts/${h.id}/console')">Local console</button>
     <button class="btn btn-sm btn-ghost" onclick="event.stopPropagation();go('#/fleet/hosts/${h.id}/settings')">Host settings</button></div></div>
  </div></td></tr>`;
 };
 return `<div class="page">
 ${head('Fleet',`${online} of ${F.HOSTS.length} hosts online · ${F.SESSIONS.filter(s=>s.state==='streaming').length} sessions running`,
  `<button class="btn btn-ghost">${icon('refresh')}Refresh</button><button class="btn btn-primary">${icon('plus')}Enroll host</button>`)}
 ${FLEET_TABS('hosts')}
 <div class="toolbar">
  <div class="segmented"><button aria-selected="true">All</button><button aria-selected="false">Online</button><button aria-selected="false">Needs attention <span class="num" style="opacity:.7">2</span></button></div>
  <div class="search">${icon('search')}<input placeholder="Filter hosts"></div>
  <div class="right"><button class="btn btn-ghost btn-sm">${icon('filter')}Group by GPU vendor</button></div>
 </div>
 ${tableCard([{l:''},{l:'Host'},{l:'GPU'},{l:'Utilisation'},{l:'Live',a:'right'},{l:'Seen',a:'right'},{l:''}],F.HOSTS.map(row).join(''))}
 </div>`;
}
function pageHostDetail(id){
 const h=F.HOSTS.find(x=>x.id===id)||F.HOSTS[0];
 const sess=F.SESSIONS.filter(s=>s.host===h.name);
 const dp=pct(h.storeTotal-h.storeFree,h.storeTotal);
 return `<div class="page">
 ${head(h.name,`${h.cpu} · ${h.ram} · uptime ${h.uptime}`,
  `${chip(h.state)}<button class="btn btn-ghost" onclick="go('#/fleet/hosts/${h.id}/console')">Local console</button><button class="btn btn-ghost">Drain</button><button class="btn" onclick="go('#/fleet/hosts/${h.id}/settings')">Settings</button>`,
  `<a onclick="go('#/fleet/hosts')">Fleet</a>${icon('chev')}<span class="mono">${h.id}</span>`)}
 ${h.state!=='online'?`<div class="note warn" style="margin-bottom:var(--s4)"><strong>${h.state==='offline'?'No heartbeat for 14 minutes.':'GPU enumeration returned 0 devices.'}</strong> Scheduling is paused for this host. Last successful heartbeat ${h.hb}.</div>`:''}
 <div class="card" style="margin-bottom:var(--s4)">
  <div class="panel-head"><span class="panel-title">Capacity</span><div class="acts"><span class="chip">${h.gpus.length} GPU${h.gpus.length>1?'s':''}</span><span class="chip">${h.sessions} session${h.sessions===1?'':'s'}</span></div></div>
  <div style="padding:var(--card-pad);display:flex;flex-direction:column;gap:var(--s6)">
   ${h.gpus.map(g=>{const vp=pct(g.vram[0],g.vram[1]);return `<div style="display:flex;gap:var(--s5);align-items:center">
    ${gauge(vp,toneC(vp))}
    <div style="flex:1;min-width:0">
     <div class="rowflex"><span style="font-weight:600;color:var(--text)">${g.n}</span><span class="cell-id">${g.v}</span><span class="chip${g.slots[0]?' chip-success':''}" style="margin-left:auto">${g.slots[0]} active</span></div>
     <div class="bar-row" style="margin-top:9px"><span>VRAM</span>${bar(g.vram[0],g.vram[1],tone(vp))}<span class="v">${g.vram[0]} / ${g.vram[1]} GB</span></div>
     <div class="bar-row" style="margin-top:6px"><span>SLOTS</span>${bar(g.slots[0],g.slots[1],tone(pct(g.slots[0],g.slots[1])))}<span class="v">${g.slots[0]} / ${g.slots[1]}</span></div>
    </div></div>`}).join('')}
   <div style="display:flex;gap:var(--s5);align-items:center">
    ${gauge(h.ramPct,toneC(h.ramPct))}
    <div style="flex:1;min-width:0">
     <div class="rowflex"><span style="font-weight:600;color:var(--text)">Memory</span><span class="cell-id">${h.ram}</span></div>
     <div class="bar-row" style="margin-top:9px"><span>USED</span>${bar(h.ramPct,100,tone(h.ramPct))}<span class="v">${Math.round(parseInt(h.ram)*h.ramPct/100)} / ${parseInt(h.ram)} GB</span></div>
     <div class="bar-row" style="margin-top:6px"><span>CPU</span>${bar(Math.round(h.ramPct*.8),100,tone(Math.round(h.ramPct*.8)))}<span class="v">${Math.round(h.ramPct*.8)}%</span></div>
    </div></div>
   <div style="display:flex;gap:var(--s5);align-items:center">
    ${gauge(dp,toneC(dp))}
    <div style="flex:1;min-width:0">
     <div class="rowflex"><span style="font-weight:600;color:var(--text)">Storage</span><span class="cell-id">agent-data</span><a style="margin-left:auto;font-size:var(--t-sm)" onclick="go('#/fleet/storage')">Managed homes</a></div>
     <div class="bar-row" style="margin-top:9px"><span>USED</span>${bar(h.storeTotal-h.storeFree,h.storeTotal,tone(dp))}<span class="v">${h.storeTotal-h.storeFree} / ${h.storeTotal} GB</span></div>
     <div class="bar-row" style="margin-top:6px"><span>FREE</span>${bar(h.storeFree,h.storeTotal,'success')}<span class="v">${h.storeFree} GB</span></div>
    </div></div>
  </div>
  <div class="table-wrap" style="border-top:1px solid var(--line)"><table class="qtable">
   ${[['Node ID',`<span class="cell-id">${h.id}-ac7d-4638-a7e8</span>`],['CPU',h.cpu],['Agent',h.agent],['Heartbeat',h.hb],['Uptime',h.uptime],['Scheduling',h.state==='online'?'accepting sessions':'paused']]
    .map(([k,v])=>`<tr><td style="color:var(--text-3);width:30%">${k}</td><td class="right primary">${v}</td></tr>`).join('')}
  </table></div>
 </div>
 <div class="card"><div class="panel-head"><span class="panel-title">Sessions on this host</span><div class="acts"><span class="chip">${sess.length}</span></div></div>
  ${sess.length?`<div class="table-wrap"><table class="qtable"><thead><tr><th>App</th><th>User</th><th>GPU</th><th>State</th><th class="right">FPS</th><th class="right">Latency</th><th class="right">Duration</th></tr></thead><tbody>
  ${sess.map(s=>`<tr class="clickable" onclick="go('#/sessions/${s.id}')"><td class="primary">${esc(s.app)}</td><td>${s.user}</td><td>${s.gpu}</td><td>${chip(s.state)}</td><td class="right num">${s.fps||'—'}</td><td class="right num">${s.lat||'—'}</td><td class="right num">${s.dur}</td></tr>`).join('')}
  </tbody></table></div>`:`<div class="empty"><h3>No sessions</h3><p>This host is not running anything right now.</p></div>`}
 </div></div>`;
}
const KNOBS={
 runtime:[
  ['Idle timeout','Stops idle sessions after this many seconds.','num','1800','1800','secs'],
  ['Adaptive bitrate','Lets sessions reduce bitrate when the network degrades.','bool',true,true],
  ['Zero-copy path','Uses the experimental GPU memory path when supported.','bool',true,false],
  ['Managed-home root (host path)','Absolute path on this host where managed homes live; empty uses the agent’s QUASAR_HOME_ROOT env.','text','/var/lib/quasar/homes','']],
 encoder:[
  ['Encoder','Selects the encoder backend used by new agent processes.','enum',['NVENC','VA-API','Software'],'NVENC','VA-API'],
  ['Render node','GPU render device path, or software rendering.','enum',['Software rendering','GeForce RTX 5090 — /dev/dri/renderD128'],'GeForce RTX 5090 — /dev/dri/renderD128','Software rendering'],
  ['CUDA device','NVENC CUDA device index.','num','0','0'],
  ['32-bit NVIDIA libs (host path)','Host directory holding 32-bit NVIDIA driver libs; auto-detected when empty.','text','','']],
 advanced:[
  ['ABR floor','Minimum adaptive bitrate before quality is considered unsustainable.','num','6000','6000','kbps'],
  ['ABR floor ratio','Fallback floor as a ratio of the selected profile bitrate.','num','0.35','0.35'],
  ['GOP length','Keyframe interval for new sessions.','num','120','120'],
  ['Encoder slices','Slice count passed to the encoder.','num','1','1'],
  ['Target usage','Encoder speed and quality hint.','enum',['Quality','Balanced','Speed'],'Balanced','Balanced'],
  ['Queue buffers','Pipeline buffering depth before encode.','num','4','4']]};
function pageHostSettings(id){
 const h=F.HOSTS.find(x=>x.id===id)||F.HOSTS[0];
 const knobRow=(k,restart)=>{
  const [label,help,type,def,val,unit]=k;
  const overridden=type==='enum'?(unit&&unit!==val):String(def)!==String(val);
  const defLabel=type==='enum'?unit:(type==='bool'?(def?'On':'Off'):(def||'unset'));
  const ctrl=type==='bool'?`<span class="switch" role="switch" aria-checked="${val}"></span>`
   :type==='enum'?`<select class="select" style="width:260px">${def.map(o=>`<option${o===val?' selected':''}>${o}</option>`).join('')}</select>`
   :`<div style="display:flex;align-items:center;gap:7px"><input class="input num" value="${val}" style="width:${type==='num'?'110px':'260px'}">${type==='num'&&unit?`<span class="hint">${unit}</span>`:''}</div>`;
  return `<div class="cset">
   <div><h3>${label} ${restart?'<span class="chip chip-warning" style="margin-left:5px">restart</span>':''}${overridden?'<span class="chip chip-accent" style="margin-left:5px">overridden</span>':''}</h3>
    <p class="hint">${help}</p>
    ${overridden?`<p class="hint" style="margin-top:4px">Default <span class="mono">${defLabel}</span> · <a onclick="return false">reset to default</a></p>`:''}</div>
   <div>${ctrl}</div></div>`;
 };
 const panel=(title,desc,rows,restart,extra)=>`<div class="card" style="margin-bottom:var(--s4)">
  <div class="panel-head"><div><span class="panel-title">${title}</span><div class="hint" style="margin-top:3px">${desc}</div></div>${extra||''}</div>
  ${rows.map(k=>knobRow(k,restart)).join('')}</div>`;
 return `<div class="page">
 ${head('Host settings',`Runtime configuration for ${h.name}. Unset values fall back to the instance default.`,
  `<button class="btn btn-ghost">Discard</button><button class="btn btn-primary">Save changes</button>`,
  `<a onclick="go('#/fleet/hosts')">Fleet</a>${icon('chev')}<a onclick="go('#/fleet/hosts/${h.id}')">${h.name}</a>${icon('chev')}<span>Settings</span>`)}
 <div class="split" style="grid-template-columns:1fr 300px">
  <div>
   <div class="note warn" style="margin-bottom:var(--s4)">Two <strong>restart-class</strong> changes are pending. Saving them restarts the node agent and ends <strong>2 live sessions</strong> on this host.</div>
   ${panel('Runtime defaults','Applies to new sessions immediately.',KNOBS.runtime,false)}
   ${panel('Encoder and GPU','Read by new agent processes — changing these requires an agent restart.',KNOBS.encoder,true)}
   ${panel('Advanced streaming tuning','Rarely changed. Wrong values here degrade every session on the host.',KNOBS.advanced,false,`<div class="acts"><button class="btn btn-sm btn-ghost">Hide advanced</button></div>`)}
  </div>
  <div style="display:flex;flex-direction:column;gap:var(--s4)">
   <div class="card card-pad"><div class="eyebrow">Host</div><h3 style="font-size:var(--t-h3);margin-top:6px">${h.name}</h3><div class="mono" style="color:var(--text-3);font-size:var(--t-xs);margin-top:3px">${h.id}</div>
    <div style="margin-top:10px">${chip(h.state)}</div>
    <div style="display:flex;flex-direction:column;gap:8px;margin-top:var(--s5);padding-top:var(--s4);border-top:1px solid var(--line);font-size:var(--t-sm)">
     <div class="rowflex" style="justify-content:space-between"><span class="hint">Agent</span><span class="mono">${h.agent}</span></div>
     <div class="rowflex" style="justify-content:space-between"><span class="hint">Heartbeat</span><span class="mono">${h.hb}</span></div>
     <div class="rowflex" style="justify-content:space-between"><span class="hint">Live sessions</span><span class="mono">${h.sessions}</span></div>
     <div class="rowflex" style="justify-content:space-between"><span class="hint">Pending restart</span>${chip('yes','warning')}</div></div>
    <button class="btn btn-sm" style="width:100%;justify-content:center;margin-top:var(--s4)">Restart agent now</button></div>
   <div class="card card-pad"><div class="eyebrow">Overrides</div><div style="font-family:var(--font-display);font-size:1.7rem;font-weight:600;margin-top:6px">3</div><div class="hint">Set on this host. Everything else follows the instance default.</div></div>
  </div></div></div>`;
}
function pageHostConsole(id){
 const h=F.HOSTS.find(x=>x.id===id)||F.HOSTS[0];
 const row=(t,p,ctrl)=>`<div class="cset"><div><h3>${t}</h3><p class="hint">${p}</p></div><div>${ctrl}</div></div>`;
 const sw=(on,dis)=>`<span class="switch" role="switch" aria-checked="${on}"${dis?' style="opacity:.45;cursor:not-allowed"':''}></span>`;
 const sel=(opts,w)=>`<select class="select" style="width:${w||'240px'}">${opts.map(o=>`<option>${o}</option>`).join('')}</select>`;
 const meta=t=>`<span class="mono" style="font-size:var(--t-xs);color:var(--text-3)">${t}</span>`;
 const grp=t=>`<div class="eyebrow" style="padding:var(--s5) var(--card-pad) 2px">${t}</div>`;
 return `<div class="page">
 ${head('Local console',`Local display on ${h.name} with an explicit per-session output topology`,
  `<button class="btn btn-ghost">Discard</button><button class="btn btn-primary">Save changes</button>`,
  `<a onclick="go('#/fleet/hosts')">Fleet</a>${icon('chev')}<a onclick="go('#/fleet/hosts/${h.id}')">${h.name}</a>${icon('chev')}<span>Local console</span>`)}
 <div class="split" style="grid-template-columns:1fr 300px">
  <div class="card">
   <div class="panel-head"><div><span class="panel-title">Console mode</span><div class="hint" style="margin-top:3px">Local display with an explicit per-session output topology.</div></div>
    <div class="acts">${sw(true)}</div></div>
   ${grp('Video')}
   ${row('Video topology','Local-only uses no encoder or WebRTC signaling resources. Dual output adds a browser stream from the same VulkanImage source.',meta('Weston · Static mode · Fullscreen'))}
   ${row('Physical output','Card-scoped DRM connector. Automatic uses Weston’s preferred connected output.',sel(['Automatic','card1-DP-1','card1-HDMI-A-1']))}
   ${row('Physical mode','Exact DRM timing identity; fractional refresh rates are preserved.',sel(['Preferred','3840×2160 @ 59.997 Hz · preferred','2560×1440 @ 143.912 Hz','1920×1080 @ 60.000 Hz'],'260px'))}
   ${grp('Streaming')}
   ${row('Also stream','Adds WebRTC video for dual output. Off is local-only.',sw(true))}
   ${row('Stream audio','Adds the WebRTC Opus audio leg when streaming is enabled.',sw(false))}
   ${grp('Local input and audio')}
   ${row('Local audio output','Host sink for console-mode audio. Quiet plays no local audio.',sel(['Auto','Quiet (no local audio)','HDMI / DisplayPort (RTX 5090)','Analog line-out']))}
   ${row('Grab local input','Exclusively grab the physical keyboard/mouse for the console session.',sw(true))}
   <div class="cset" style="grid-template-columns:1fr;align-items:stretch">
    <div><h3>Input devices</h3><p class="hint">What the host reports, and what gets passed through to the container. Pick device classes to stay broad, or select individual devices.</p></div>
    <div style="margin-top:var(--s3)">
     <div class="segmented" style="margin-bottom:var(--s4)"><button aria-selected="true">Auto \u00b7 by class</button><button aria-selected="false">Specific devices</button><button aria-selected="false">None</button></div>
     <div style="display:flex;gap:8px;flex-wrap:wrap;margin-bottom:var(--s4)">
      ${[['Keyboards',true],['Mice',true],['Controllers',true],['Touch',false],['Tablets',false],['Audio jacks',false]].map(([l,on])=>`<button class="chip${on?' chip-accent':''}" style="cursor:pointer;height:26px">${on?'\u2713 ':''}${l}</button>`).join('')}
     </div>
     <div class="table-wrap" style="border:1px solid var(--line);border-radius:var(--r-sm)"><table class="qtable">
      <thead><tr><th style="width:34px"></th><th>Device</th><th>Class</th><th>Path</th><th class="right">State</th></tr></thead><tbody>
      ${[['Keychron K2','Keyboard','/dev/input/event3','matched by class'],['Logitech G502','Mouse','/dev/input/event5','matched by class'],['Xbox Wireless Controller','Controller','/dev/input/event9','matched by class'],['Wacom Intuos S','Tablet','/dev/input/event11','not passed'],['HDA Intel PCH front-mic','Audio jack','/dev/input/event2','not passed']]
       .map(([n,c,p,s])=>{const on=s!=='not passed';return `<tr><td><span style="display:inline-block;width:14px;height:14px;border-radius:4px;border:1px solid ${on?'var(--accent)':'var(--line-3)'};background:${on?'var(--accent)':'transparent'}"></span></td>
       <td class="primary">${n}</td><td>${c}</td><td><span class="cell-id">${p}</span></td>
       <td class="right">${on?chip('passed through','success'):'<span style="color:var(--text-4);font-size:var(--t-xs)">not passed</span>'}</td></tr>`}).join('')}
      </tbody></table></div>
     <div class="hint" style="margin-top:9px">Class rules follow hot-plug: a controller connected later is passed through automatically. Individually selected devices are pinned by path and will not follow a re-enumeration.</div>
    </div></div>
   ${grp('Startup')}
   ${row('Default app','App auto-launched on console start.',sel(['None',...window.QDATA.APPS.map(a=>a.name)]))}
   ${row('Default user','Owner of auto-started console sessions. Required for auto-start on display.',sel(['None',...window.QDATA.USERS.map(u=>u.name+' ('+u.mail+')')]))}
   ${row('Auto-start on display','Auto-launch the console session when a display connects.',sw(true))}
   ${row('Auto-connect controller','Auto-attach a connected controller to the console session.',sw(true))}
  </div>
  <div style="display:flex;flex-direction:column;gap:var(--s4)">
   <div class="card card-pad"><div class="eyebrow">Host</div><h3 style="font-size:var(--t-h3);margin-top:6px">${h.name}</h3><div class="sub mono" style="color:var(--text-3);font-size:var(--t-xs);margin-top:3px">${h.id}</div></div>
   <div class="card card-pad"><div class="eyebrow">Overrides</div><div style="font-family:var(--font-display);font-size:1.7rem;font-weight:600;margin-top:6px">2</div><div class="hint">Unsaved field changes.</div></div>
   <div class="card card-pad"><div class="eyebrow">Reported capabilities</div>
    <div style="display:flex;flex-direction:column;gap:9px;margin-top:10px;font-size:var(--t-xs);color:var(--text-3)">
     <div>Connectors: <span class="mono">DP-1, HDMI-A-1</span></div>
     <div style="border-left:2px solid var(--line-2);padding-left:9px;display:flex;flex-direction:column;gap:2px">
      <span class="cell-id" style="align-self:flex-start">card1-DP-1</span>
      <span class="mono">/dev/dri/renderD128</span><span>connected</span>
      <span>active 3840×2160 @ 59.997 Hz</span></div>
     <div>Audio sinks: HDMI / DisplayPort, Analog line-out</div>
     <div>Input devices: 5 reported \u00b7 3 passed through</div></div></div>
  </div></div></div>`;
}
function pageStorage(){
 const total=F.STORAGE.reduce((n,s)=>n+s.size,0);
 return `<div class="page">
 ${head('Fleet',`${F.STORAGE.length} managed homes · ${total} GB provisioned`,`<button class="btn btn-ghost">${icon('refresh')}Refresh</button><button class="btn">Reclaim pending</button>`)}
 ${FLEET_TABS('storage')}
 <div class="grid g4" style="margin-bottom:var(--s5)">
  ${[['Managed homes',F.STORAGE.length,'across 4 hosts'],['Total size',total+' GB','of 2 304 GB allocated'],['Active',F.STORAGE.filter(s=>s.state==='active').length,'attached to a user'],['Pending cleanup',F.STORAGE.filter(s=>s.state!=='active').length,'11 GB reclaimable']]
   .map(([k,v,m])=>`<div class="card card-pad"><div class="eyebrow">${k}</div><div style="font-family:var(--font-display);font-size:1.7rem;font-weight:600;margin-top:8px">${v}</div><div style="font-size:var(--t-sm);color:var(--text-3);margin-top:5px">${m}</div></div>`).join('')}
 </div>
 <div class="toolbar"><div class="search">${icon('search')}<input placeholder="Filter by user or host"></div>
  <select class="select"><option>All providers</option><option>Docker volume</option><option>Local directory</option></select>
  <div class="right"><span class="label" style="font-size:var(--t-sm)">New homes use</span><select class="select"><option>Automatic</option><option>Docker volume</option><option>Local directory</option></select></div></div>
 ${tableCard([{l:'User'},{l:'Host'},{l:'Provider'},{l:'Usage'},{l:'Size',a:'right'},{l:'Quota',a:'right'},{l:'State'},{l:''}],
  F.STORAGE.map(s=>{const p=pct(s.size,s.quota);return `<tr>
   <td class="primary">${s.user}</td><td>${s.host.replace('quasar-','')}</td><td>${s.provider}</td>
   <td style="width:180px"><div class="bar-row"><span></span>${bar(s.size,s.quota,tone(p))}<span class="v">${p}%</span></div></td>
   <td class="right num">${s.size} GB</td><td class="right num" style="color:var(--text-3)">${s.quota} GB</td>
   <td>${chip(s.state)}</td><td class="cell-actions"><button class="icon-btn">${icon('dots')}</button></td></tr>`}).join(''))}
 </div>`;
}
// Images lives under Library in the IA (images back runtime presets), but the page code stays here.
const islug=n=>n.toLowerCase().replace(/[^a-z0-9]+/g,'-').replace(/^-|-$/g,'');
const imgShort=n=>n.replace('quasar-node-','node ');
const imgRollout=im=>{
 const total=F.HOSTS.length;
 if(!im.hosts.length)return '<span style="color:var(--text-4)">—</span>';
 const ready=im.hosts.filter(h=>h[1]==='ready').length;
 const named=im.hosts.map(h=>h[0]);
 const notes=im.hosts.filter(h=>h[1]!=='ready').map(h=>`${imgShort(h[0])} ${h[1]}`);
 const missing=F.HOSTS.filter(h=>!named.includes(h.name)).map(h=>imgShort(h.name));
 if(missing.length)notes.push(missing.length>2?`not on ${missing.length} hosts`:'not on '+missing.join(', '));
 return `<div class="bar-row" style="max-width:132px"><span class="num" style="min-width:34px;color:var(--text-2)">${ready}/${total}</span>${bar(ready,total,im.hosts.some(h=>h[1]!=='ready')?'warning':'success')}</div>
  <div class="sub" style="margin-top:4px">${notes.length?notes.join(' · '):'ready on every host'}</div>`;
};
const imgVersion=im=>{
 if(im.state==='installing')return `<div>${im.version}</div><div class="sub" style="color:var(--info-text)">installing</div>`;
 if(!im.installed)return `<div>${im.version}</div><div class="sub">not installed</div>`;
 if(im.state==='update')return `<div>${im.version}</div><div class="sub" style="color:var(--warning-text)">running ${im.installed}</div>`;
 return `<div>${im.version}</div><div class="sub">up to date</div>`;
};
const imgBar=im=>{
 const g=(ic,t,cls)=>`<button class="gbtn ${cls||''}" title="${t}" aria-label="${t}" onclick="event.stopPropagation()">${icon(ic)}</button>`;
 const up=im.state==='update';
 return `<div class="gbar">
  ${up?g('refresh',`Update to ${im.version}`,'todo'):g('refresh','Re-ensure on every host')}
  ${im.installed||im.state==='installing'?g('trash','Uninstall everywhere','rm'):g('download','Install on every host','todo')}
  ${g('pin',im.pinned?'Unpin version':'Pin to '+(im.installed||im.version),im.pinned?'on':'')}
  <button class="gbtn" title="Open image" aria-label="Open image" onclick="go('#/library/images/${islug(im.name)}')">${icon('chev')}</button>
 </div>`;
};
function pageImages(){
 const installed=F.IMAGES.filter(i=>i.installed).length;
 const updates=F.IMAGES.filter(i=>i.state==='update').length;
 return `<div class="page">
 ${head('Library','Container images mirrored from the quasar-images catalog. Installs, updates and uninstalls apply to every connected host.',
  `<button class="btn btn-primary">${icon('refresh')}Sync catalog</button>`)}
 ${LIB_TABS('images')}
 <div class="card card-pad" style="margin-bottom:var(--s4);display:flex;gap:var(--s7);align-items:center;flex-wrap:wrap">
  <div><div class="eyebrow">Update policy</div>
   <div class="segmented" style="margin-top:8px"><button aria-selected="false">Manual</button><button aria-selected="false">Notify</button><button aria-selected="true">Auto</button></div></div>
  <div class="hint" style="max-width:430px">After a sync, every installed, unpinned image with a newer catalog version re-adopts and re-ensures automatically. Pinned images are left alone.</div>
  <div class="right" style="margin-left:auto;text-align:right"><div class="eyebrow">Catalog</div>
   <div style="font-size:var(--t-sm);color:var(--text-2);margin-top:6px">${F.IMAGES.length} images · ${installed} installed${updates?` · <span style="color:var(--warning-text)">${updates} update available</span>`:''}</div>
   <div class="hint">Last synced 8 Aug 2026, 09:14</div></div>
 </div>
 <div class="toolbar"><div class="search">${icon('search')}<input placeholder="Filter catalog"></div>
  <div class="segmented"><button aria-selected="true">All <span class="num" style="opacity:.7">${F.IMAGES.length}</span></button><button aria-selected="false">Installed <span class="num" style="opacity:.7">${installed}</span></button><button aria-selected="false">Updates <span class="num" style="opacity:.7">${updates}</span></button></div>
  <select class="select"><option>All hosts</option>${F.HOSTS.map(h=>`<option>${h.name}</option>`).join('')}</select>
 </div>
 ${tableCard([{l:'Image'},{l:'Version'},{l:'Rollout'},{l:''}],
  F.IMAGES.map(im=>`<tr class="clickable" onclick="go('#/library/images/${islug(im.name)}')">
   <td><div class="primary">${esc(im.name)}</div><div class="sub mono" style="margin-top:2px">${esc(im.ref)}</div></td>
   <td class="num" style="color:var(--text)">${imgVersion(im)}</td>
   <td style="white-space:normal">${imgRollout(im)}</td>
   <td class="cell-actions">${imgBar(im)}</td></tr>`).join(''))}
 </div>`;
}
function pageImageDetail(slug){
 const im=F.IMAGES.find(x=>islug(x.name)===slug)||F.IMAGES[0];
 const appId=n=>{const a=F.APPS.find(x=>x.name===n);return a?a.id:'';};
 const total=F.HOSTS.length,ready=im.hosts.filter(h=>h[1]==='ready').length;
 const fact=(k,v)=>`<div class="ae-fact"><span>${k}</span><span>${v}</span></div>`;
 const lead=im.state==='update'?`<button class="btn btn-primary">${icon('refresh')}Update to ${im.version}</button>`
  :im.state==='installing'?`<button class="btn btn-ghost" disabled>Installing…</button>`
  :!im.installed?`<button class="btn btn-primary">${icon('download')}Install on every host</button>`
  :`<button class="btn btn-ghost">${icon('refresh')}Re-ensure</button>`;
 return `<div class="page">
 ${head(esc(im.name),`${esc(im.kind)} · from ${esc(im.provider)} · ${ready} of ${total} hosts ready`,
  `${lead}<button class="btn btn-ghost">${icon('pin')}${im.pinned?'Unpin':'Pin version'}</button>`,
  `<a onclick="go('#/library/images')">Library</a>${icon('chev')}<a onclick="go('#/library/images')">Images</a>${icon('chev')}<span>${esc(im.name)}</span>`)}
 <div class="editor">
  <div style="display:flex;flex-direction:column;gap:var(--s4)">
   <div class="card card-pad">
    <p style="font-size:var(--t-sm);color:var(--text-2);line-height:1.55;margin:0 0 var(--s4);max-width:70ch">${esc(im.desc)}</p>
    <div class="ae-facts">
     ${fact('Reference',`<span class="mono">${esc(im.ref)}</span>`)}
     ${fact('Digest',`<span class="mono">${esc(im.digest)}</span>`)}
     ${fact('Catalog version',`<span class="num">${esc(im.version)}</span>`)}
     ${fact('Installed version',im.installed?`<span class="num">${esc(im.installed)}</span>`:'<span style="color:var(--text-4)">not installed</span>')}
     ${fact('Last pulled',esc(im.pulled))}
    </div>
   </div>
   <div class="card"><div class="panel-head"><span class="panel-title">Per host</span><div class="acts hint">${ready} of ${total} ready</div></div>
    <div class="table-wrap"><table class="qtable"><thead><tr><th>Host</th><th>State</th><th class="right">Version</th><th class="right">Sessions</th></tr></thead><tbody>
    ${F.HOSTS.map(h=>{const f=im.hosts.find(x=>x[0]===h.name);const st=f?f[1]:'absent';
     return `<tr class="clickable" onclick="go('#/fleet/hosts/${h.id}')">
      <td><div class="rowflex">${sdot(st)}<span class="primary">${h.name}</span></div></td>
      <td${st==='stale'?' style="color:var(--warning-text)"':st==='absent'?' style="color:var(--text-4)"':''}>${st==='absent'?'not installed':st}</td>
      <td class="right num">${st==='absent'?'—':st==='stale'?im.installed:st==='pulling'?'—':im.installed||im.version}</td>
      <td class="right num">${h.sessions}</td></tr>`;}).join('')}
    </tbody></table></div></div>
   <div class="card"><div class="panel-head"><span class="panel-title">Used by</span><div class="acts hint">${im.presets.length} presets · ${im.apps.length} apps</div></div>
    ${im.presets.length||im.apps.length?`<div class="card-pad" style="display:flex;gap:var(--s7);flex-wrap:wrap">
     <div style="flex:1 1 220px"><div class="eyebrow" style="margin-bottom:9px">Runtime presets</div>
      ${im.presets.length?im.presets.map(p=>`<div class="ae-fact"><a onclick="go('#/library/presets')">${esc(p)}</a><span class="sub">preset</span></div>`).join(''):'<div class="sub">None</div>'}</div>
     <div style="flex:1 1 220px"><div class="eyebrow" style="margin-bottom:9px">Apps</div>
      ${im.apps.length?im.apps.map(a=>`<div class="ae-fact"><a onclick="go('#/library/apps/${appId(a)}')">${esc(a)}</a><span class="sub">app</span></div>`).join(''):'<div class="sub">None</div>'}</div>
    </div>`:`<div class="card-pad"><div class="note">Nothing points at this image.${im.installed?' Uninstalling it reclaims the space on every host.':''}</div></div>`}
   </div>
  </div>
  <div class="ae-rail">
   <div class="card card-pad">
    <div class="eyebrow">Rollout</div>
    <div class="bar-row" style="margin-top:10px"><span class="num" style="min-width:34px;color:var(--text-2)">${ready}/${total}</span>${bar(ready,total,im.hosts.some(h=>h[1]!=='ready')?'warning':'success')}</div>
    <div class="ae-facts" style="margin-top:var(--s4)">
     ${fact('State',im.state==='update'?'<span style="color:var(--warning-text)">update available</span>':im.state==='installing'?'<span style="color:var(--info-text)">installing</span>':im.installed?'up to date':'not installed')}
     ${fact('Version pinned',im.pinned?'yes':'no')}
     ${fact('Update policy',`<a onclick="go('#/library/images')">Auto</a>`)}
    </div>
   </div>
   ${im.installed||im.state==='installing'?`<button class="btn btn-danger" style="width:100%;justify-content:center">${icon('trash')}Uninstall everywhere</button>
   <p class="hint">Removes the image from every connected host. Presets and apps that point at it stop launching until it is reinstalled.</p>`:''}
  </div>
 </div></div>`;
}
Object.assign(window,{pageHosts,pageHostDetail,pageHostConsole,pageHostSettings,pageStorage,pageImages,pageImageDetail});
