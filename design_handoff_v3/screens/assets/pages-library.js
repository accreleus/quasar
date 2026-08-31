// Library: Apps · App editor · Runtime presets · Sources. Streaming: Launch + Stream profiles.
const L=window.QDATA;
const LIB_TABS=n=>tabs([
 {id:'apps',label:'Apps',count:L.APPS.length,go:'#/library/apps'},
 {id:'presets',label:'Runtime presets',count:L.PRESETS.length,go:'#/library/presets'},
 {id:'images',label:'Images',count:L.IMAGES.length,go:'#/library/images'},
 {id:'sources',label:'Sources',count:2,go:'#/library/sources'}],n);
function pageSources(){
 const src=(name,desc,on,extra,meta,act)=>`<div style="display:flex;gap:var(--s5);align-items:flex-start;padding:var(--card-pad);border-bottom:1px solid var(--line)">
  <div style="flex:1;min-width:0">
   <div class="rowflex"><span style="font-family:var(--font-display);font-size:var(--t-h3);font-weight:600">${name}</span></div>
   <div class="hint" style="margin-top:5px;max-width:60ch">${desc}</div>
   ${meta?`<div style="margin-top:10px;font-size:var(--t-sm);color:var(--text-2)">${meta}</div>`:''}
   ${extra||''}
  </div>
  <div style="display:flex;align-items:center;gap:var(--s2);flex:none">
   ${act||''}
   <span class="switch" role="switch" aria-checked="${on}" style="margin-left:var(--s3)"></span></div></div>`;
 return `<div class="page">
 ${head('Library','Where catalog content and cover art come from. Everything a source discovers lands in Apps.')}
 ${LIB_TABS('sources')}
 <div class="eyebrow" style="margin-bottom:10px">Content sources</div>
 <div class="card">
  ${src('Steam','Discovers titles from the Steam library installed on your hosts. Importing one creates an app that inherits the Proton GPU preset.',true,'',
   `42 titles discovered · <strong>5 imported</strong> · last scan 21 minutes ago`,
   `<button class="btn btn-sm btn-ghost">${icon('refresh')}Scan now</button><button class="btn btn-sm" onclick="go('#/library/apps')">Review 37 pending ${icon('chev')}</button>`)}
  ${src('RomM','Connects a RomM instance and exposes its ROM collections as apps, one emulator runtime per platform.',false,`<div class="grid g2" style="margin-top:var(--s4);max-width:520px"><div class="field"><label class="label">RomM URL</label><input class="input mono" placeholder="https://romm.local"></div><div class="field"><label class="label">API key</label><input class="input mono" type="password" placeholder="••••••••••••"></div></div>`,'','')}
  ${src('Manual apps','Apps you define by hand against a runtime preset. Always on — this is how anything outside a source gets into the catalog.',true,'','10 apps defined by hand',`<button class="btn btn-sm" onclick="go('#/library/apps')">Open Apps ${icon('chev')}</button>`)}
 </div>
 <div class="eyebrow" style="margin:var(--s7) 0 10px">Artwork providers</div>
 <div class="card">
  <div style="display:flex;gap:var(--s5);align-items:flex-start;padding:var(--card-pad)">
   <div style="flex:1;min-width:0">
    <div class="rowflex"><span style="font-family:var(--font-display);font-size:var(--t-h3);font-weight:600">SteamGridDB</span>${chip('not configured')}</div>
    <div class="hint" style="margin-top:5px;max-width:66ch">Lets the control plane look up cover artwork for apps in your catalogue. Without it, artwork can still be uploaded by hand and every app falls back to its gradient tile.</div>
    <div class="field" style="margin-top:var(--s4);max-width:560px"><label class="label">API key</label>
     <div style="display:flex;gap:8px"><input class="input mono" type="password" placeholder="Paste the key"><button class="btn">Save key</button></div>
     <span class="hint">Stored encrypted and never shown again — this server can use it, but cannot display it. <span class="mono">QUASAR_STEAMGRIDDB_API_KEY</span> remains supported as a fallback. <a href="#">Where to get one</a></span></div>
   </div>
   <div style="flex:none"><span class="switch" role="switch" aria-checked="false"></span></div></div>
 </div>
 <div class="note" style="margin-top:var(--s4)">Artwork providers are tried in order. Apps with no match keep their gradient tile.</div>
 </div>`;
}

const STR_TABS=n=>tabs([
 {id:'launch',label:'Launch profiles',count:L.LAUNCH.length,go:'#/streaming/launch'},
 {id:'profiles',label:'Stream profiles',count:L.STREAMP.length,go:'#/streaming/profiles'}],n);
const PENDING=[{name:'Stardew Valley',size:'1.2 GB',proton:'verified'},{name:'Deep Rock Galactic',size:'14.6 GB',proton:'verified'},{name:'Satisfactory',size:'22.1 GB',proton:'verified'},{name:'Hollow Knight',size:'9.4 GB',proton:'unverified'}];
function pageApps(q){
 const preset=q?decodeURIComponent((q.match(/preset=([^&]+)/)||[])[1]||''):'';
 const rows=preset?L.APPS.filter(a=>a.preset===preset):L.APPS;
 return `<div class="page">
 ${head('Library',`${L.APPS.filter(a=>a.enabled).length} of ${L.APPS.length} apps enabled · 37 discovered titles not yet imported`,`<button class="btn btn-ghost">${icon('refresh')}Scan sources</button><button class="btn btn-primary">${icon('plus')}Add app</button>`)}
 ${LIB_TABS('apps')}
 <div class="toolbar">
  <div class="segmented"><button aria-selected="true">All <span class="num" style="opacity:.7">${L.APPS.length}</span></button><button aria-selected="false">Games</button><button aria-selected="false">Desktops</button><button aria-selected="false">Disabled <span class="num" style="opacity:.7">1</span></button><button aria-selected="false">Pending import <span class="num" style="opacity:.7">37</span></button></div>
  <div class="search">${icon('search')}<input placeholder="Filter apps"></div>
  <div class="right"><select class="select"><option>All sources</option><option>Steam</option><option>Manual</option></select><button class="btn btn-ghost btn-sm">Bulk edit</button></div>
 </div>
 ${preset?`<div class="toolbar"><span class="chip chip-accent" style="height:26px">Runtime preset: ${esc(preset)}<button onclick="go('#/library/apps')" style="border:0;background:none;color:inherit;cursor:pointer;margin-left:4px;font-size:11px">✕</button></span><span class="hint">${rows.length} of ${L.APPS.length} apps</span></div>`:''}
 ${tableCard([{l:'App'},{l:'Kind'},{l:'Source'},{l:'Runtime preset'},{l:'Launch profile'},{l:'Sessions · 30d',a:'right'},{l:'Enabled',a:'right'},{l:''}],
  (preset?'':PENDING.map(p=>`<tr style="background:var(--accent-soft)">
   <td><div class="rowflex"><span style="width:26px;height:26px;border-radius:var(--r-xs);border:1px dashed var(--line-3);display:grid;place-content:center;font-family:var(--font-display);font-size:11px;font-weight:700;color:var(--text-3);flex:none">${p.name[0]}</span>
    <div class="stack"><span class="primary">${esc(p.name)}</span><span class="sub">${p.size} installed · Proton ${p.proton}</span></div></div></td>
   <td>${chip('Game','accent')}</td><td>Steam</td>
   <td colspan="3" style="color:var(--text-3)">Not imported — importing applies the Proton GPU preset</td>
   <td class="right">${chip('pending','warning')}</td>
   <td class="cell-actions"><button class="btn btn-sm">Import</button></td></tr>`).join(''))+
  rows.map(a=>`<tr class="clickable" style="${a.enabled?'':'opacity:.6'}" onclick="go('#/library/apps/${a.id}')">
   <td><div class="rowflex"><span style="width:26px;height:26px;border-radius:var(--r-xs);background:var(--brand-grad);display:grid;place-content:center;font-family:var(--font-display);font-size:11px;font-weight:700;color:#fff;flex:none">${a.name[0]}</span>
    <div class="stack"><span class="primary">${esc(a.name)}</span><span class="sub mono">${a.img}</span></div></div></td>
   <td>${chip(a.kind,a.kind==='Game'?'accent':'neutral')}</td>
   <td>${a.src}</td>
   <td><a onclick="event.stopPropagation();go('#/library/apps?preset='+encodeURIComponent('${a.preset}'))">${a.preset}</a></td>
   <td><a onclick="event.stopPropagation();go('#/streaming/launch')">${a.launch}</a></td>
   <td class="right num">${a.sessions30}</td>
   <td class="right"><span class="switch" role="switch" aria-checked="${a.enabled}" onclick="event.stopPropagation()"></span></td>
   <td class="cell-actions"><button class="icon-btn" onclick="event.stopPropagation()">${icon('dots')}</button></td></tr>`).join(''))}
 </div>`;
}
const PRESET_DETAIL={
 'Proton GPU':{desc:'Wine/Proton runtime for Windows titles.',img:'ghcr.io/quasar/proton:9.0',args:['--fullscreen'],env:[['PROTON_VERSION','9.0'],['DXVK_HUD','0'],['PULSE_LATENCY_MSEC','60']],home:true,path:'/home/quasar',mounts:['/srv/steam:/steam']},
 'Native Linux':{desc:'Minimal Vulkan + PipeWire base for native builds.',img:'ghcr.io/quasar/native:3',args:[],env:[['SDL_VIDEODRIVER','wayland']],home:true,path:'/home/quasar',mounts:[]},
 'Workstation 4K':{desc:'Creative tools with CUDA and OpenCL runtimes.',img:'ghcr.io/quasar/blender:4.2',args:['--factory-startup'],env:[['CUDA_VISIBLE_DEVICES','0'],['QUASAR_TARGET_FPS','60']],home:true,path:'/home/quasar',mounts:['/mnt/projects:/projects','/mnt/cache:/cache']},
 'Windows VM':{desc:'QEMU guest with GPU passthrough.',img:'ghcr.io/quasar/winvm:11',args:[],env:[['OVMF','1'],['CPU_PIN','8-15']],home:false,path:'',mounts:['/var/lib/quasar/vm:/vm']}};
function openPreset(name){
 const p=L.PRESETS.find(x=>x.name===name)||L.PRESETS[0];
 const d=PRESET_DETAIL[p.name]||{desc:'',img:p.img,args:[],env:[],home:true,path:'/home/quasar',mounts:[]};
 const apps=L.APPS.filter(a=>a.preset===p.name);
 const del=`<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" style="width:13px;height:13px"><path d="M4 4l8 8M12 4l-8 8" stroke-linecap="round"/></svg>`;
 document.getElementById('drawer').innerHTML=`<div class="dscrim" onclick="closeDrawer()"></div><aside class="drawer">
 <div class="dw-head"><div><div class="eyebrow mono" style="text-transform:none;letter-spacing:0">runtime preset</div><h2 style="margin-top:4px">${esc(p.name)}</h2></div>
  <button class="icon-btn" style="margin-left:auto" onclick="closeDrawer()">${del}</button></div>
 <div class="dw-body">
  <div class="fsec"><div class="fs-label"><h4>Identity</h4><p>How the preset appears when picking one on an app.</p></div>
   <div class="fs-fields">
    <div class="field"><label class="label">Name</label><input class="input" value="${esc(p.name)}"></div>
    <div class="field"><label class="label">Description</label><textarea class="textarea" rows="2" style="font-family:var(--font-ui)">${esc(d.desc)}</textarea></div></div></div>
  <div class="fsec"><div class="fs-label"><h4>Container</h4><p>Image, arguments and environment every app using this preset starts from.</p></div>
   <div class="fs-fields">
    <div class="field"><label class="label">Image</label><input class="input mono" value="${d.img}" placeholder="quasar-dev:latest"></div>
    <div class="field"><span class="label">Launch arguments</span><div class="kv">
     ${d.args.map((a,i)=>`<div class="kv-row" style="width:100%"><input class="input mono" value="${esc(a)}" placeholder="--headless"><button class="kv-del">${del}</button></div>`).join('')}
     <button class="btn btn-sm btn-ghost">+ Add argument</button><span class="hint">Apps append their own after these.</span></div></div>
    <div class="field"><span class="label">Environment variables</span><div class="kv">
     ${d.env.map(([k,v])=>`<div class="kv-row" style="width:100%"><input class="input mono" value="${esc(k)}" placeholder="KEY" style="flex:1"><input class="input mono" value="${esc(v)}" placeholder="value" style="flex:1.4"><button class="kv-del">${del}</button></div>`).join('')}
     <button class="btn btn-sm btn-ghost">+ Add variable</button><span class="hint">An app setting the same key overrides the value here.</span></div></div></div></div>
  <div class="fsec"><div class="fs-label"><h4>Storage</h4><p>Defaults for apps using this preset. An app can override them.</p></div>
   <div class="fs-fields">
    <label class="rowflex" style="gap:10px;font-size:var(--t-sm);font-weight:600"><span class="switch" role="switch" aria-checked="${d.home}"></span>Managed home</label>
    ${d.home?`<div class="field"><label class="label">Mount path inside container</label><input class="input mono" value="${d.path}" placeholder="/home/quasar"></div>`:''}
    <div class="field"><span class="label">Mounts</span><div class="kv">
     ${d.mounts.map(m=>`<div class="kv-row" style="width:100%"><input class="input mono" value="${esc(m)}" placeholder="/host/path:/container/path"><button class="kv-del">${del}</button></div>`).join('')}
     <button class="btn btn-sm btn-ghost">+ Add mount</button><span class="hint">Apps append their own. Two mounts on one container path is a misconfiguration, not a merge.</span></div></div></div></div>
  <div class="fsec"><div class="fs-label"><h4>Used by</h4><p>Apps inheriting this preset. Editing it changes all of them.</p></div>
   <div class="fs-fields"><div style="display:flex;gap:7px;flex-wrap:wrap">${apps.length?apps.map(a=>`<button class="chip chip-accent" style="cursor:pointer" onclick="closeDrawer();go('#/library/apps/${a.id}')">${esc(a.name)}</button>`).join(''):'<span class="hint">Not used by any app yet.</span>'}</div>
    <span class="hint">A preset in use cannot be deleted.</span></div></div>
 </div>
 <div class="dw-foot"><button class="btn btn-danger"${apps.length?' disabled style="opacity:.45;cursor:not-allowed" title="In use — remove it from every app first"':''}>Delete</button>
  <div style="margin-left:auto;display:flex;gap:8px"><button class="btn btn-ghost" onclick="closeDrawer()">Cancel</button><button class="btn btn-primary" onclick="closeDrawer()">Save changes</button></div></div></aside>`;
}
function closeDrawer(){document.getElementById('drawer').innerHTML='';}
function pagePresets(){
 return `<div class="page">
 ${head('Library','Shared container configuration an app inherits rather than repeating',`<button class="btn btn-primary">${icon('plus')}New preset</button>`)}
 ${LIB_TABS('presets')}
 <div class="toolbar">
  <div class="segmented"><button aria-selected="true">All <span class="num" style="opacity:.7">${L.PRESETS.length}</span></button><button aria-selected="false">In use <span class="num" style="opacity:.7">4</span></button><button aria-selected="false">Unused</button></div>
  <div class="search">${icon('search')}<input placeholder="Filter presets"></div>
  <div class="right"><select class="select"><option>All images</option>${[...new Set(L.PRESETS.map(p=>p.img))].map(i=>`<option>${i}</option>`).join('')}</select>
   <select class="select"><option>GPU: any</option><option>Required</option><option>Optional</option></select></div>
 </div>
 ${tableCard([{l:'Preset'},{l:'Image'},{l:'Environment',a:'right'},{l:'Mounts',a:'right'},{l:'GPU'},{l:'Used by',a:'right'},{l:''}],
  L.PRESETS.map(p=>`<tr class="clickable" onclick="openPreset('${p.name}')">
   <td class="primary">${p.name}</td>
   <td><span class="cell-id">${p.img}</span></td>
   <td class="right num">${p.env} keys</td>
   <td class="right num">${p.mounts}</td>
   <td>${chip(p.gpu?'required':'optional',p.gpu?'success':'neutral')}</td>
   <td class="right"><a onclick="event.stopPropagation();go('#/library/apps?preset=${encodeURIComponent(p.name)}')">${p.used} app${p.used>1?'s':''}</a></td>
   <td class="cell-actions" onclick="event.stopPropagation()">${menu([{label:'Edit preset',fn:`openPreset('${p.name}')`},{label:'Duplicate preset'},'-',{label:'Delete preset',danger:true}])}</td></tr>`).join(''))}
 </div>`;
}
function pageLaunch(){
 return `<div class="page">
 ${head('Streaming','What a user picks, and the quality chain behind it',`<button class="btn btn-primary">${icon('plus')}New launch profile</button>`)}
 ${STR_TABS('launch')}
 <div class="card card-pad" style="margin-bottom:var(--s4);display:flex;gap:var(--s7);align-items:center;flex-wrap:wrap">
  <div><div class="eyebrow">Default profile</div><select class="select" style="margin-top:7px">${L.LAUNCH.map(p=>`<option${p.def?' selected':''}>${p.name}</option>`).join('')}</select></div>
  <div style="max-width:420px"><div class="rowflex" style="justify-content:space-between;gap:var(--s5)"><div><div class="label">Let users choose a profile</div><div class="hint">Otherwise every session uses the default</div></div><span class="switch" role="switch" aria-checked="true"></span></div></div>
 </div>
 <div class="grid g2">
 ${L.LAUNCH.map(p=>`<div class="card"><div class="panel-head"><span class="panel-title">${p.name}</span>
   <div class="acts">${p.def?chip('default','accent'):''}<span class="chip">${p.used} app${p.used>1?'s':''}</span>${menu([{label:'Rename profile'},{label:'Duplicate profile'},p.def?{label:'Already the default'}:{label:'Set as default'},'-',{label:'Delete profile',danger:true}])}</div></div>
  <div style="padding:var(--s3) var(--card-pad) var(--card-pad)">
  ${p.rungs.map((r,i)=>{const last=i===p.rungs.length-1;return `<div class="rung${last?' locked':''}">
   <span class="rank">${i+1}</span>
   <span class="nm">${r.split(' · ')[0]}</span>
   <span class="br">${r.split(' · ')[1]}</span>
   <span class="ctl">
    <button title="Move up"${i===0?' disabled style="opacity:.3"':''}>↑</button>
    <button title="Move down"${last?' disabled style="opacity:.3"':''}>↓</button>
    <button class="rm" title="${last?'The last rung must be H.264':'Remove rung'}">✕</button></span></div>`}).join('')}
  <div style="display:flex;gap:8px;align-items:center;margin-top:12px">
   <select class="select" style="flex:1"><option>Add a stream profile…</option>${L.STREAMP.map(s=>`<option>${s.name} · ${s.res} · ${s.br} Mb/s</option>`).join('')}</select>
   <button class="btn btn-sm">${icon('plus')}Add</button></div>
  <div class="hint" style="margin-top:10px">Falls through in order. The last rung must be H.264 — every browser can decode it.</div>
  </div></div>`).join('')}
 </div></div>`;
}
function pageStreamProfiles(){
 const byCodec={};L.STREAMP.forEach(p=>(byCodec[p.codec]=byCodec[p.codec]||[]).push(p));
 return `<div class="page">
 ${head('Streaming','The encode rungs themselves, grouped by codec',`<button class="btn btn-primary">${icon('plus')}New stream profile</button>`)}
 ${STR_TABS('profiles')}
 ${Object.entries(byCodec).map(([codec,rows])=>`<div class="card" style="margin-bottom:var(--s4)">
  <div class="panel-head"><span class="panel-title">${codec}</span><div class="acts"><span class="chip${codec==='H.264'?'':' chip-accent'}">${codec==='H.264'?'universal fallback':'hardware required'}</span><span class="chip">${rows.length} rungs</span></div></div>
  <div class="table-wrap"><table class="qtable"><thead><tr><th>Profile</th><th>Resolution</th><th class="right">FPS</th><th class="right">Bitrate</th><th class="right">ABR floor</th><th>Encoder</th><th>Browser</th><th class="right">Used by</th><th></th></tr></thead><tbody>
  ${rows.map(p=>`<tr><td><span class="mono primary">${p.name}</span></td><td class="num">${p.res}</td><td class="right num">${p.fps}</td>
   <td class="right num">${p.br} Mb/s</td><td class="right num" style="color:var(--text-3)">${p.floor} Mb/s</td>
   <td>${p.enc}</td><td style="color:var(--text-3)">${p.browser}</td>
   <td class="right num">${p.used} profile${p.used>1?'s':''}</td>
   <td class="cell-actions"><button class="icon-btn">${icon('dots')}</button></td></tr>`).join('')}
  </tbody></table></div></div>`).join('')}
 </div>`;
}
Object.assign(window,{pageApps,pagePresets,pageSources,pageLaunch,pageStreamProfiles,openPreset,closeDrawer});
