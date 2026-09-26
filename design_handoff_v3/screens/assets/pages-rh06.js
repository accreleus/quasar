// RH-06 console surfaces (#354): Quasar-owned machines, their services and
// updates. Renders into fleet-rh06-v3.html; every function is a pure render
// over the mock data below, like the other pages-*.js section renderers.
//
// Everything here reuses console-v3.css classes and ui.js helpers. The few
// classes this file needs that console-v3.css does not have (.modal*,
// .snippet, .diag, .rel-*) are defined in fleet-rh06-v3.html's own <style>,
// built only from existing tokens — see that file's header comment.
//
// Names, versions, digests and addresses are fictional placeholders.

const RH={
 cp:{v:'0.5.2',commit:'3f9a2c1',schema:88,floor:'0.5.0',machine:'living-room-pc'},
 next:{v:'0.6.0',commit:'b71e04d',schema:91,date:'24 Sep 2026, 16:40'},
 ns:['ghcr.io/accreleus','registry.example:5000/quasar-dev'],
 seedImg:'ghcr.io/accreleus/quasar-recovery@sha256:4c1d…e90a',
 cpUrl:'https://quasar.example:8443'};

// The five machines of a household fleet, one per state the surfaces need.
const RH_HOSTS=[
 {id:'5a1c90e2',name:'living-room-pc',shape:'Combined host',state:'online',cpu:'AMD Ryzen 7 7700X',ram:'64 GB',ramPct:38,gpus:[{n:'GeForce RTX 4080 Super',v:'NVIDIA',vram:[9.4,16],slots:[1,2]}],storeFree:212,storeTotal:480,sessions:1,hb:'3s ago',uptime:'9d 2h',agent:'0.5.2',actor:'0.5.2',seed:'0.5.0',seedOwner:'manager',flag:null},
 {id:'8d03f7b1',name:'gpu-host-2',shape:'GPU host',state:'online',cpu:'Intel Core i5-13600K',ram:'32 GB',ramPct:44,gpus:[{n:'Radeon RX 7800 XT',v:'AMD',vram:[6.2,16],slots:[2,2]}],storeFree:140,storeTotal:240,sessions:2,hb:'2s ago',uptime:'3d 7h',agent:'0.5.2',actor:'0.5.2',seed:null,flag:'seed'},
 {id:'c4e21a09',name:'gpu-host-3',shape:'GPU host',state:'online',cpu:'AMD Ryzen 5 5600X',ram:'32 GB',ramPct:31,gpus:[{n:'GeForce RTX 3070',v:'NVIDIA',vram:[3.1,8],slots:[1,2]}],storeFree:88,storeTotal:240,sessions:1,hb:'4s ago',uptime:'21d 5h',agent:'0.4.1',actor:'0.4.1',seed:'0.4.1',seedOwner:'hand',flag:'floor'},
 {id:'3b8e5d17',name:'gpu-host-4',shape:'GPU host',state:'online',cpu:'AMD Ryzen 7 5800X3D',ram:'32 GB',ramPct:47,gpus:[{n:'GeForce RTX 4060 Ti',v:'NVIDIA',vram:[7.8,16],slots:[2,2]}],storeFree:176,storeTotal:480,sessions:2,hb:'3s ago',uptime:'12d 8h',agent:'0.5.1',actor:'0.5.2',seed:'0.5.0',seedOwner:'hand',flag:null},
 {id:'91b7d3e4',name:'study-pc',shape:'GPU host',state:'online',cpu:'Intel Core i7-12700',ram:'32 GB',ramPct:22,gpus:[{n:'GeForce RTX 4070',v:'NVIDIA',vram:[0,12],slots:[0,2]}],storeFree:301,storeTotal:480,sessions:0,hb:'5s ago',uptime:'1d 1h',agent:'0.5.2',actor:'0.5.2',seed:'0.5.0',seedOwner:'manager',flag:'conflict'},
 {id:'e6f0a2c8',name:'gpu-host-5',shape:'GPU host',state:'offline',cpu:'AMD Ryzen 5 7600',ram:'16 GB',ramPct:0,gpus:[{n:'Radeon RX 7600',v:'AMD',vram:[0,8],slots:[0,1]}],storeFree:96,storeTotal:120,sessions:0,hb:'3d ago',uptime:'—',agent:'0.5.2',actor:'0.5.2',seed:'0.5.0',seedOwner:'hand',flag:null}];
// Fleet totals the Releases view states, derived so they cannot disagree with
// the host list.
const rhAgentSummary=()=>{const on=RH_HOSTS.filter(h=>h.agent===RH.cp.v).length,older=RH_HOSTS.length-on;
 return `${RH_HOSTS.length} hosts · ${on} on v${RH.cp.v}${older?` · ${older} older`:''}`;};
const rhHost=id=>RH_HOSTS.find(h=>h.id===id||h.name===id);
// The machine Add host enrolled a moment ago (not yet in the table above).
const RH_NEW={id:'f2a61c3d',name:'gpu-host-6',shape:'GPU host',state:'online',cpu:'Intel Core i5-12400',ram:'16 GB',ramPct:12,gpus:[{n:'Radeon RX 6600',v:'AMD',vram:[0,8],slots:[0,1]}],storeFree:210,storeTotal:240,sessions:0,hb:'2s ago',uptime:'0d 1h',agent:'0.5.2',actor:'0.5.2',seed:'0.5.0',seedOwner:'hand',flag:null};

const RH_FLAG={seed:['no seed','warning'],floor:['must update','warning'],conflict:['owner conflict','warning']};
const rhFlagChip=h=>h.flag?chip(RH_FLAG[h.flag][0],RH_FLAG[h.flag][1]):'';
const muted=t=>`<span style="color:var(--text-4)">${t}</span>`;
const dis='disabled style="opacity:.5;cursor:not-allowed"';

/* ---------------------------------------------------------------------------
   shared pieces
   ------------------------------------------------------------------------ */

// A command the operator runs, shown verbatim with a Copy button (the product's
// CopyableCommand / enroll-snippet, which this mirrors).
const snippet=(caption,text,sub)=>`<div style="display:flex;flex-direction:column;gap:7px">
 ${caption?`<div><div class="eyebrow">${caption}</div>${sub?`<div class="hint" style="margin-top:3px">${sub}</div>`:''}</div>`:''}
 <div class="snippet"><pre>${esc(text)}</pre><button class="btn btn-sm btn-ghost">${icon('copy')}Copy</button></div></div>`;

// The one place identifiers appear: a closed-by-default diagnostic affordance.
const diag=(label,lines,open)=>`<details class="diag"${open?' open':''}><summary>${icon('chev')}${label}</summary><pre>${esc(lines)}</pre></details>`;

// A mock-only stand-in for the fixed scrim, so a dialog can sit in page flow.
const stage=(inner,kind)=>`<div class="stage${kind?' stage-'+kind:''}">${inner}</div>`;

const modal=(title,body,foot,w)=>`<div class="modal" role="dialog" aria-label="${esc(title)}" style="max-width:${w||460}px">
 <div class="modal-head"><h3>${title}</h3><button class="icon-btn" aria-label="Close"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M4 4l8 8M12 4l-8 8" stroke-linecap="round"/></svg></button></div>
 <div class="modal-body">${body}</div>
 ${foot?`<div class="modal-foot">${foot}</div>`:''}</div>`;

const railFact=(k,v)=>`<div class="rel-fact"><span>${k}</span><span>${v}</span></div>`;
const RH_TABS=n=>tabs([
 {id:'hosts',label:'Hosts',count:RH_HOSTS.length,go:'#'},
 {id:'storage',label:'Storage',count:6,go:'#'},
 {id:'releases',label:'Releases',count:1,go:'#'}],n);

/* ---------------------------------------------------------------------------
   Surface 1 — the per-machine service inventory
   ------------------------------------------------------------------------ */

// One machine's services. `mode`: the database mode (own | external | none).
function rhServices(h,o={}){
 const cpHere=h.shape!=='GPU host',agentHere=h.shape!=='Control-only host';
 const stale=o.stale,unknown=o.unknown;
 const run=unknown?chip('unknown'):stale?chip('as of 13:48'):chip('running','success');
 const vers=(v,sub)=>unknown?muted('—'):`<div class="stack"><span class="num" style="color:var(--text)">v${v}</span>${sub?`<span class="sub">${sub}</span>`:''}</div>`;
 const own=t=>unknown?muted('—'):t;
 const row=(name,desc,ver,owner,state)=>`<tr><td><div class="stack"><span class="primary">${name}</span><span class="sub" style="white-space:normal;max-width:34ch">${desc}</span></div></td><td>${ver}</td><td>${owner}</td><td class="right">${state}</td></tr>`;
 const none=(what)=>[muted('—'),muted('—'),`<span class="hint">${what}</span>`];
 const seedRow=h.seed||unknown
  ?row('Seed','Makes sure the recovery actor exists. Never updated by Quasar.',vers(h.seed||'0.5.0',h.seedOwner==='hand'?'started by the one-line command':'declared in an external manager'),own(h.seedOwner==='hand'?'You (docker run)':'External manager'),o.seedUnknown?chip('unknown'):run)
  :row('Seed','Makes sure the recovery actor exists. Never updated by Quasar.',muted('—'),muted('—'),chip('not found','warning'));
 const dbMode=o.db||(cpHere?'own':'none');
 const dbRow=dbMode==='own'?row('Database','Quasar’s own Postgres, created at install. Quasar does not update it; it dumps it before a migrating update.',unknown?muted('—'):`<div class="stack"><span class="num" style="color:var(--text)">Postgres 16.4</span><span class="sub">Quasar’s own</span></div>`,own('Quasar'),run)
  :dbMode==='external'?row('Database','Your own database. Quasar only uses it: it never dumps, restores, resets or upgrades it.',`<div class="stack"><span style="color:var(--text)">Your own</span><span class="sub mono">db.example:5432</span></div>`,'You',unknown?chip('unknown'):chip('reachable','success'))
  :row('Database','No database runs on a GPU host.',...none('none on this machine'));
 const conflict=o.conflict?`<tr><td><div class="stack"><span class="primary" style="color:var(--warning-text)">quasar-node-agent-1</span><span class="sub" style="white-space:normal;max-width:34ch">Looks like a node agent, but this installation did not create it.</span></div></td><td>${muted('—')}</td><td>Another owner</td><td class="right">${chip('in the way','warning')}</td></tr>`:'';
 const rows=[
  seedRow,
  row('Recovery actor','Creates, updates and recovers the services on this machine, and itself.',vers(h.actor,o.actorSub||''),own('Quasar'),o.actorFloor?chip('must update','warning'):run),
  dbRow,
  cpHere?row('Control plane','Accounts, the console, scheduling and signaling.',vers(RH.cp.v,`commit ${RH.cp.commit} · schema ${RH.cp.schema}`),own('Quasar'),run):row('Control plane','Runs on living-room-pc.',...none('not on this machine')),
  agentHere?row('Node agent','Runs this machine’s GPUs and sessions.',vers(h.agent,o.agentSub||''),own('Quasar'),o.agentFloor?chip('must update','warning'):run):row('Node agent','No GPU sessions run here.',...none('not on this machine')),
  conflict].join('');
 return `<div class="card"${stale?' style="opacity:.92"':''}>
  <div class="panel-head"><div><span class="panel-title">Services on this machine</span><div class="hint" style="margin-top:3px">${o.headHint||'Each Quasar service has one owner. On this machine every service but the seed is owned by its recovery actor.'}</div></div>
   <div class="acts">${chip(h.shape)}</div></div>
  ${o.note?`<div style="padding:var(--s4) var(--card-pad)">${o.note}</div>`:''}
  <div class="table-wrap"><table class="qtable"><thead><tr><th>Service</th><th>Version</th><th>Owner</th><th class="right">State</th></tr></thead><tbody>${rows}</tbody></table></div>
  ${o.foot===false?'':`<div class="card-pad" style="border-top:1px solid var(--line);display:flex;gap:var(--s5);align-items:center;flex-wrap:wrap">${o.foot||rhRemoveFoot(h)}</div>`}
 </div>`;
}
// Remove host lives with the services it removes (the image-detail pattern:
// a danger button and the hint that says what it does).
function rhRemoveFoot(h){
 if(h.shape!=='GPU host')return `<p class="hint" style="margin:0;max-width:70ch;line-height:1.55">This machine runs the control plane, so it is not removed from here. To uninstall it, run the uninstall command on the machine; it keeps the database, machine state and homes unless you ask it to purge.</p>`;
 return `<p class="hint" style="margin:0;flex:1;min-width:260px;line-height:1.55">Removing drains the host, then stops and removes its node agent and recovery actor. Homes and data stay on the machine.</p>
  <button class="btn btn-danger btn-sm">${icon('trash')}Remove host</button>`;
}

// The host-detail page head, as admin-console-v3's pageHostDetail draws it.
// managed=false (below the floor): only Drain stays; settings are not offered.
function rhDetailHead(h,extra,managed=true){
 return `${head(h.name,`${h.shape} · ${h.cpu} · ${h.ram} · uptime ${h.uptime}`,
  `${chip(h.state)}${extra||''}${managed?'<button class="btn btn-ghost">Local console</button>':''}<button class="btn btn-ghost">Drain</button>${managed?'<button class="btn">Settings</button>':''}`,
  `<a>Fleet</a>${icon('chev')}<span class="mono">${h.id}</span>`)}`;
}
const rhTrailer=`<div class="hint" style="margin-top:var(--s4)">Capacity and Sessions cards follow, unchanged from admin-console-v3.</div>`;

function pageRhInventory(variant){
 const h=variant==='gpu'?rhHost('gpu-host-4'):variant==='unknown'?RH_NEW:variant==='error'?rhHost('gpu-host-2'):rhHost('living-room-pc');
 if(variant==='combined')return `<div class="page">${rhDetailHead(h)}${rhServices(h,{agentSub:''})}${rhTrailer}</div>`;
 if(variant==='gpu')return `<div class="page">${rhDetailHead(h)}${rhServices(h,{agentSub:'older than the control plane · update from Releases'})}${rhTrailer}</div>`;
 if(variant==='unknown')return `<div class="page">${rhDetailHead(h)}${rhServices(h,{unknown:true,foot:false,headHint:'Versions and owners appear once this machine’s recovery actor reports.',
   note:`<div class="note">gpu-host-6 enrolled 20 seconds ago and has not reported its services yet.</div>`})}</div>`;
 return `<div class="page">${rhDetailHead(h)}${rhServices(h,{stale:true,foot:false,headHint:'Last report from 13:48. Nothing here is acted on until the recovery actor answers again.',
   note:`<div class="note warn"><strong>Could not read this machine’s services.</strong> Its recovery actor has not answered for 14 minutes, so the list below is its last report. The node agent is connected; sessions are unaffected.${diag('Details','status request to recovery actor: timed out after 5 s\nlast status: 2026-09-25T13:48:02Z\nhost: 8d03f7b1')}</div>`})}</div>`;
}

// Hosts table: admin-console-v3's pageHosts row, plus the machine's shape,
// an attention chip, and a Services column in the expansion row.
function pageRhHosts(){
 const row=h=>{
  const v=h.gpus.reduce((a,g)=>a+g.vram[0],0),vt=h.gpus.reduce((a,g)=>a+g.vram[1],0);
  const su=h.gpus.reduce((a,g)=>a+g.slots[0],0),st=h.gpus.reduce((a,g)=>a+g.slots[1],0);
  const sp=pct(su,st),vp=pct(v,vt),dp=pct(h.storeTotal-h.storeFree,h.storeTotal);
  const dim=h.state==='offline'?'opacity:.55':'';
  const op=h.name==='living-room-pc';
  const u=(l,val,total,txt,p)=>`<div class="bar-row"><span>${l}</span>${bar(val,total,tone(p))}<span class="v">${txt}</span></div>`;
  const svc=(k,val)=>`<div class="exp-fact"><span>${k}</span><span>${val}</span></div>`;
  return `<tr class="clickable" style="${dim}">
   <td style="width:34px;padding-right:0"><button class="exp-btn" aria-expanded="${op}" title="Show capacity, services and storage">${icon('chev')}</button></td>
   <td><div class="rowflex">${sdot(h.state)}<span class="primary">${h.name}</span>${rhFlagChip(h)}</div>
       <div class="sub mono" style="margin-top:2px;padding-left:17px">${h.id} · <span style="font-family:var(--font-ui)">${h.shape}</span>${h.state!=='online'?' · '+h.state:''}</div></td>
   <td><div class="stack"><span>${h.gpus[0].n}</span><span class="sub">${h.gpus[0].v}</span></div></td>
   <td style="min-width:230px"><div class="u2">
     ${u('GPU',su,st,`${su}/${st}`,sp)}${u('VRAM',v,vt,`${Math.round(v)}/${vt}`,vp)}
     ${u('RAM',h.ramPct,100,`${h.ramPct}%`,h.ramPct)}${u('DISK',h.storeTotal-h.storeFree,h.storeTotal,`${dp}%`,dp)}
    </div></td>
   <td class="right num">${h.sessions}</td>
   <td class="right num" style="color:${h.state==='online'?'var(--success-text)':'var(--danger-text)'}">${h.hb.replace(' ago','')}</td>
   <td class="cell-actions">${menu(h.flag==='floor'?[{label:'Open host'},{label:'Update to v'+RH.cp.v},'-',{label:'Drain'},{label:'Remove host',danger:1}]:[{label:'Open host'},{label:'Local console'},{label:'Host settings'},'-',{label:'Drain'},{label:'Remove host',danger:1}])}</td></tr>
  ${op?`<tr class="exp-row"><td colspan="7"><div class="exp-in">
   <div><div class="eyebrow">Hardware</div>
    <div class="exp-fact"><span>CPU</span><span>${h.cpu}</span></div>
    <div class="exp-fact"><span>Memory</span><span>${h.ram} · ${h.ramPct}% used</span></div>
    <div class="exp-fact"><span>Uptime</span><span>${h.uptime}</span></div></div>
   <div><div class="eyebrow">Services</div>
    ${svc('Seed',`<span class="num">v${h.seed}</span> · external manager`)}
    ${svc('Recovery actor',`<span class="num">v${h.actor}</span>`)}
    ${svc('Database','Quasar’s own')}
    ${svc('Control plane',`<span class="num">v${RH.cp.v}</span>`)}
    ${svc('Node agent',`<span class="num">v${h.agent}</span>`)}</div>
   <div><div class="eyebrow">GPUs and slots</div>
    ${h.gpus.map((g,i)=>`<div class="exp-fact"><span>${g.n} #${i}</span><span class="num">${g.slots[0]}/${g.slots[1]} slots · ${g.vram[0]}/${g.vram[1]} GB</span></div>`).join('')}</div>
   <div><div class="eyebrow">Storage</div>
    <div class="exp-fact"><span>Used</span><span class="num">${h.storeTotal-h.storeFree} / ${h.storeTotal} GB</span></div>
    <div class="exp-fact"><span>Free</span><span class="num">${h.storeFree} GB</span></div></div>
   <div><div class="eyebrow">Actions</div>
    <div style="display:flex;flex-direction:column;gap:7px;align-items:flex-start;margin-top:2px">
     <button class="btn btn-sm btn-ghost">Open host</button><button class="btn btn-sm btn-ghost">Local console</button><button class="btn btn-sm btn-ghost">Host settings</button></div></div>
  </div></td></tr>`:''}`;
 };
 const online=RH_HOSTS.filter(h=>h.state==='online').length;
 return `<div class="page">
 ${head('Fleet',`${online} of ${RH_HOSTS.length} hosts online · ${RH_HOSTS.reduce((n,h)=>n+h.sessions,0)} sessions running`,
  `<button class="btn btn-ghost">${icon('refresh')}Refresh</button><button class="btn btn-primary">${icon('plus')}Add host</button>`)}
 ${RH_TABS('hosts')}
 <div class="toolbar">
  <div class="segmented"><button aria-selected="true">All</button><button aria-selected="false">Online</button><button aria-selected="false">Needs attention <span class="num" style="opacity:.7">4</span></button></div>
  <div class="search">${icon('search')}<input placeholder="Filter hosts"></div>
 </div>
 ${tableCard([{l:''},{l:'Host'},{l:'GPU'},{l:'Utilisation'},{l:'Live',a:'right'},{l:'Seen',a:'right'},{l:''}],RH_HOSTS.map(row).join(''))}
 </div>`;
}

/* ---------------------------------------------------------------------------
   Surface 2 — seed-missing and owner-conflict warnings
   ------------------------------------------------------------------------ */
function pageRhWarning(variant){
 if(variant==='seed'){
  const h=rhHost('gpu-host-2');
  return `<div class="page">${rhDetailHead(h)}
  <div class="note warn" style="margin-bottom:var(--s4)"><strong>No seed found on gpu-host-2.</strong> Quasar is running normally, but if this machine’s recovery actor is ever deleted, nothing will re-create it. Start the seed again the way you first started it — in your external manager, or with the command from Add host. The machine keeps its identity and nothing else changes.</div>
  ${rhServices(h,{})}</div>`;}
 if(variant==='seed-unknown'){
  const h=rhHost('gpu-host-4');
  return `<div class="page">${rhDetailHead(h)}
  <div class="note" style="margin-bottom:var(--s4)"><strong>Seed not checked yet.</strong> The recovery actor on gpu-host-4 looks for the seed every few minutes; the last look was before it restarted. This clears on its own.</div>
  ${rhServices(h,{foot:false,seedUnknown:true})}</div>`;}
 const h=rhHost('study-pc');
 const box=`<div class="note warn" style="margin-bottom:var(--s4)"><strong>Another owner’s container is in the way on study-pc.</strong> <span class="mono">quasar-node-agent-1</span> looks like a Quasar node agent, but this installation did not create it — it is probably left from an older Compose install. Quasar never stops, renames or removes a container it did not create, so it will not update this machine while that container exists. Remove it on study-pc (for example, take down the old Compose stack), then check again.
   <div style="display:flex;gap:8px;margin-top:10px"><button class="btn btn-sm">${icon('refresh')}Check again</button></div>
   ${diag('Details',`readiness check: owner_conflict
container: quasar-node-agent-1 (3e8b0c91d2f4)
image: ghcr.io/accreleus/quasar-node-agent:0.3.0
labels: com.docker.compose.project=quasar
missing: io.quasar.installation=inst-7d21…
blocks: platform updates on this machine`,variant==='conflict-open')}</div>`;
 if(variant==='conflict-error')return `<div class="page">${rhDetailHead(h)}
  <div class="note warn" style="margin-bottom:var(--s4)"><strong>Still in the way.</strong> <span class="mono">quasar-node-agent-1</span> was found again at 14:21, so nothing has changed. If you removed a stack, check that its containers are gone: stopped containers count too.
   <div style="display:flex;gap:8px;margin-top:10px"><button class="btn btn-sm">${icon('refresh')}Check again</button></div></div>
  ${rhServices(h,{conflict:true,foot:false})}</div>`;
 return `<div class="page">${rhDetailHead(h)}${box}${rhServices(h,{conflict:true,foot:false})}</div>`;
}

/* ---------------------------------------------------------------------------
   Surface 3 — below the floor: "must update before it can be managed"
   ------------------------------------------------------------------------ */
function pageRhFloor(variant){
 const h=rhHost('gpu-host-3');
 const upd=`<button class="btn btn-primary">${icon('download')}Update to v${RH.cp.v}</button>`;
 if(variant==='actor'){
  const a={...h,agent:'0.5.2'};
  return `<div class="page">${rhDetailHead(a,'',false)}
  <div class="note warn" style="margin-bottom:var(--s4);display:flex;gap:var(--s5);align-items:center;flex-wrap:wrap"><div style="flex:1;min-width:280px"><strong>gpu-host-3 must update before it can be managed.</strong> Its recovery actor is v0.4.1; this control plane manages v${RH.cp.floor} and newer. Sessions keep running. The only thing offered to this host is an update, which replaces the recovery actor and ends no sessions.</div>${upd}</div>
  ${rhServices(a,{actorFloor:true,actorSub:`below v${RH.cp.floor}`,foot:false})}</div>`;}
 if(variant==='failed')return `<div class="page">${rhDetailHead(h,'',false)}
  <div class="note warn" style="margin-bottom:var(--s4);display:flex;gap:var(--s5);align-items:center;flex-wrap:wrap"><div style="flex:1;min-width:280px"><strong>The update did not finish; gpu-host-3 still must update.</strong> As every update does, it replaced the recovery actor first, which is now v0.5.2. The new node agent then never became healthy, so the recovery actor put node agent v0.4.1 back. Its session ended when the node agent was replaced.${diag('Details','attempt: 1c7e…a4 · outcome: failed, restored\nreason: unhealthy\ncomponents: recovery-actor 0.4.1 → 0.5.2 (succeeded, kept), node-agent 0.4.1 → 0.5.2 (failed, restored to 0.4.1)')}</div><button class="btn btn-primary">${icon('refresh')}Try again</button></div>
  ${rhServices({...h,actor:'0.5.2'},{agentFloor:true,agentSub:`below v${RH.cp.floor}`,foot:false})}</div>`;
 if(variant==='unknown')return `<div class="page">${rhDetailHead({...h})}
  <div class="note" style="margin-bottom:var(--s4)"><strong>Version not reported.</strong> gpu-host-3 has not said which release it runs, so the console cannot tell whether this control plane manages it. Nothing is offered until it reports.</div>
  ${rhServices(h,{unknown:true,foot:false})}</div>`;
 return `<div class="page">${rhDetailHead(h,'',false)}
  <div class="note warn" style="margin-bottom:var(--s4);display:flex;gap:var(--s5);align-items:center;flex-wrap:wrap"><div style="flex:1;min-width:280px"><strong>gpu-host-3 must update before it can be managed.</strong> Its node agent and recovery actor are v0.4.1; this control plane manages v${RH.cp.floor} and newer. It keeps running sessions, but the only thing offered to it is an update. Updating ends its 1 live session.</div>${upd}</div>
  ${rhServices(h,{agentFloor:true,actorFloor:true,agentSub:`below v${RH.cp.floor}`,actorSub:`below v${RH.cp.floor}`,foot:false})}</div>`;
}

/* ---------------------------------------------------------------------------
   Fleet ▸ Releases, extended (installed inventory, targets, developer apply)
   ------------------------------------------------------------------------ */
const REL_CAT={Security:['SEC','var(--danger-text)'],Fixed:['FIX','var(--info-text)'],Added:['NEW','var(--success-text)'],Changed:['CHG','var(--warning-text)']};
const RH_NOTES=[
 {c:'Added',t:'Hosts can be added with a node name bound to the command',refs:[]},
 {c:'Changed',t:'Session history keeps per-codec start times',refs:[]},
 {c:'Fixed',t:'A drained host resumes placement when its drain is cancelled',refs:[]}];
const relEntry=n=>{const[tag,col]=REL_CAT[n.c];return `<details class="rel-e"><summary><span class="rel-c" style="color:${col}">${tag}</span><span class="rel-t">${n.t}</span><span class="rel-x">${icon('chev')}</span></summary></details>`;};
// Releases ▸ Installed. The control plane's own machine has no host row when it
// is control-only, so its inventory lives here: each service with its version,
// then owner · state, like the host page's services card.
// o.external: the operator's own database (a control-only install, 4 GPU hosts).
// o.state: 'ok' | 'unknown' (not reported yet) | 'error' (actor not answering).
function rhInstalled(o={}){
 const ext=o.external,st=o.state||'ok';
 const stTxt=st==='error'?'as of 13:48':'running';
 const svc=(k,ver,owner,state)=>`<div class="rel-fact"><span>${k}</span><span>${st==='unknown'&&ver?muted('—'):ver}<div class="hint" style="margin-top:2px">${st==='unknown'&&ver?'not reported yet':`${owner}${state?' · '+state:''}`}</div></span></div>`;
 const v=x=>`<span class="num">v${x}</span>`;
 return `<div class="card card-pad"><div class="eyebrow">Installed</div>
  <div style="margin-top:9px">
   ${railFact('Control plane',v(RH.cp.v))}
   ${railFact('Commit',`<a class="mono" style="font-size:var(--t-xs)">${RH.cp.commit}</a>`)}
   ${railFact('Schema',`<span class="num">${RH.cp.schema}</span>`)}
   ${railFact('Node agents',ext?`4 hosts<div class="hint" style="margin-top:2px">all on v${RH.cp.v}</div>`:rhAgentSummary().replace(/^(\d+ hosts) · (.*)$/,'$1<div class="hint" style="margin-top:2px;line-height:1.5">$2</div>'))}
  </div>
  <div class="eyebrow" style="margin-top:var(--s5)">This machine</div>
  <div class="hint" style="margin-top:4px">${ext?'attic-server · Control-only host':'living-room-pc · Combined host'}</div>
  ${st==='error'?`<div class="note warn" style="margin-top:8px;font-size:var(--t-xs)"><strong>Could not read this machine’s services.</strong> Its recovery actor has not answered for 14 minutes; below is its last report. The control plane is running — it is serving this page.</div>`:''}
  ${st==='unknown'?`<div class="note" style="margin-top:8px;font-size:var(--t-xs)">This machine’s recovery actor has not reported its services yet.</div>`:''}
  <div style="margin-top:6px">
   ${svc('Seed',v('0.5.0'),'External manager',stTxt)}
   ${svc('Recovery actor',v(RH.cp.v),'Quasar',stTxt)}
   ${ext?svc('Database','Your own','You',st==='error'?'as of 13:48':'reachable'):svc('Database','Quasar’s own','Quasar',stTxt)}
   <div class="rel-fact"><span>Control plane</span><span>${v(RH.cp.v)}<div class="hint" style="margin-top:2px">Quasar · running</div></span></div>
   ${ext?svc('Node agent','',muted('none on this machine'),''):svc('Node agent',v(RH.cp.v),'Quasar',stTxt)}
  </div>
  ${ext?`<p class="hint" style="margin:10px 0 0;line-height:1.5">Quasar only uses your database at <span class="mono">db.example</span>. It never dumps, restores or upgrades it.</p>`:''}
 </div>`;
}
function rhTargets(){
 return `<div class="card card-pad"><div class="eyebrow">Targets</div>
  <div class="hint" style="margin-top:4px">Evaluated against v${RH.next.v}. Control plane goes first; hosts follow in sequence. Each machine’s recovery actor is updated before its other services.</div>
  <div style="margin-top:9px">
   ${railFact('Control plane',chip('ready','success'))}
   <div class="rel-fact" style="align-items:center"><span>Node agents</span><span class="rowflex" style="gap:8px"><span class="num" style="color:var(--text-2)">4/${RH_HOSTS.length}</span>${bar(4,RH_HOSTS.length,'warning')}</span></div>
  </div>
  <div style="margin-top:7px">
   <div class="rowflex" style="justify-content:space-between;gap:8px;padding:4px 0"><a style="font-size:var(--t-xs)">gpu-host-3</a><span class="hint" style="font-size:var(--t-xs)">must update first · included</span></div>
   <div class="rowflex" style="justify-content:space-between;gap:8px;padding:4px 0"><a style="font-size:var(--t-xs)">study-pc</a><span class="hint" style="font-size:var(--t-xs);color:var(--warning-text)">another owner’s container in the way</span></div>
   <div class="rowflex" style="justify-content:space-between;gap:8px;padding:4px 0"><a style="font-size:var(--t-xs)">gpu-host-5</a><span class="hint" style="font-size:var(--t-xs)">no heartbeat</span></div>
  </div>
  <div style="margin-top:var(--s4)"><a style="font-size:var(--t-sm)">Per-host detail</a></div></div>`;
}
const rhDevCard=`<div class="card card-pad"><div class="eyebrow">Developer apply</div>
  <div class="hint" style="margin-top:6px;line-height:1.5">Apply a build that is not a release, by digest, from an allowed image namespace. Admins only; offered only on Quasar-owned machines.</div>
  <button class="btn btn-sm" style="margin-top:var(--s4)">Developer apply…</button></div>`;
function rhUpdateBanner(){
 return `<div class="card card-pad" style="margin-bottom:var(--s4);display:flex;gap:var(--s6);align-items:center;flex-wrap:wrap">
  <div style="flex:1;min-width:260px"><div class="eyebrow">Update available</div>
   <div class="rowflex" style="gap:9px;margin-top:7px"><span style="font-family:var(--font-display);font-size:1.45rem;font-weight:600">v${RH.cp.v} <span style="color:var(--text-4);font-weight:400">→</span> v${RH.next.v}</span><span class="chip chip-warning">changes the database</span></div>
   <div class="hint" style="margin-top:6px">Published ${RH.next.date} · 1 added · 1 changed · 1 fixed</div></div>
  <div style="max-width:360px"><div class="hint" style="line-height:1.55">This release changes the database, so it is never applied unattended. The update waits for every session to end, and Quasar dumps its database before the control plane moves. Each host’s sessions end when that host is updated.</div></div>
 </div>`;
}
function pageRhReleases(o={}){
 return `<div class="page">
 ${head('Fleet','stable channel · last checked 13m ago · next check Mon 02:00 UTC',`<button class="btn btn-ghost">${icon('refresh')}Check now</button><button class="btn btn-primary">${icon('download')}Update Quasar</button>`)}
 ${RH_TABS('releases')}
 ${o.banner||rhUpdateBanner()}
 <div class="split" style="grid-template-columns:1fr 300px">
  <div><div class="card" style="margin-bottom:var(--s4)">
   <div class="panel-head" style="align-items:flex-start"><div><div class="rowflex" style="gap:9px"><span style="font-family:var(--font-display);font-size:1.25rem;font-weight:600;color:var(--text)">v${RH.next.v}</span><span class="chip chip-accent">latest</span></div>
    <div class="sub" style="margin-top:4px">${RH.next.date} · <a class="mono" style="font-size:var(--t-xs)">${RH.next.commit}</a> · 1 added · 1 changed · 1 fixed</div></div>
    <div class="acts"><a class="btn btn-sm btn-ghost">View on GitHub</a></div></div>
   ${RH_NOTES.map(relEntry).join('')}</div>
   <div class="hint" style="padding:2px 4px">Earlier releases follow, unchanged from releases-v3.</div></div>
  <div style="display:flex;flex-direction:column;gap:var(--s4)">
   ${o.rail||`${rhInstalled()}${rhTargets()}${rhDevCard}`}
  </div></div></div>`;
}

/* ---------------------------------------------------------------------------
   Surface 4 — Add host
   ------------------------------------------------------------------------ */
function rhAddHost(state){
 const t2=state==='stack';
 const tabsHtml=`<div class="tabs" style="margin:0 0 var(--s5)"><button class="tab${t2?'':' active'}">One-line command</button><button class="tab${t2?' active':''}">Dockge or Arcane</button></div>`;
 const pinned=`<div class="ae-facts" style="margin-bottom:var(--s4)"><div class="ae-fact"><span>Pinned certificate</span><span class="mono" style="font-size:var(--t-xs)">SHA256 7A:3F:…:C2:19</span></div></div>`;
 const opts=(name,exp,lock)=>`<div style="display:grid;grid-template-columns:minmax(0,1fr) 170px;gap:var(--s4);margin-bottom:var(--s4)">
   <div class="field"><label class="label">Node name <span class="hint" style="font-weight:400">optional</span></label><input class="input" value="${name}" placeholder="any name"${lock?' '+dis:''}><span class="hint">Binds the command to this name. Use an existing host’s name to re-enroll it and keep its history.</span></div>
   <div class="field"><label class="label">Expires</label><select class="select"${lock?' '+dis:''}><option>${exp}</option><option>in 6 hours</option><option>in 1 day</option><option>in 7 days</option><option>in 30 days</option></select></div></div>`;
 const cmd=`curl -fsSL -k --pinnedpubkey 'sha256//EXAMPLEpinEXAMPLEpinEXAMPLEpinEXAMPLEpin=' ${RH.cpUrl}/enroll-host.sh | QUASAR_ENROLLMENT='qenr1.EXAMPLE-ENROLLMENT-STRING' sudo bash`;
 const foot=`<button class="btn btn-ghost">Close</button>`;
 let body;
 if(state==='options'){
  body=`${tabsHtml}<p class="hint" style="margin:0 0 var(--s4);font-size:var(--t-sm);line-height:1.55">One command adds a machine with Docker and a GPU. Create it here, run it on that machine as root, and the host appears in the table when it has enrolled.</p>
  ${opts('gpu-host-6','in 1 hour')}${pinned}
  <button class="btn btn-primary">Create command</button>`;
 } else if(state==='ready'){
  body=`${tabsHtml}${opts('gpu-host-6','in 1 hour',true)}
  ${snippet('Run on the new host as root',cmd,'Single use · expires at 15:42 · only for gpu-host-6')}
  <p class="hint" style="margin:var(--s4) 0 0;line-height:1.6">The command checks the host — render node, virtual input, user namespaces, the app-container AppArmor profile — and offers to fix what is missing. Then it starts Quasar’s seed and waits until the host is enrolled. It writes no compose file, no environment file and no install directory; running it again on an enrolled machine changes nothing. <span class="mono" style="white-space:nowrap">--pinnedpubkey</span> makes curl trust only this control plane’s key, so <span class="mono" style="white-space:nowrap">-k</span> here is not “trust anything”.</p>
  <details class="diag"><summary>${icon('chev')}Show the enrollment string</summary><pre>qenr1.EXAMPLE-ENROLLMENT-STRING</pre></details>`;
 } else if(state==='stack'){
  const yaml=`services:
  quasar-seed:
    image: ${RH.seedImg}
    command: seed
    restart: unless-stopped
    environment:
      QUASAR_ROLE: gpu
      QUASAR_HOME_ROOT: /srv/quasar/homes
      QUASAR_ENROLLMENT: qenr1.EXAMPLE-ENROLLMENT-STRING
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock`;
  body=`${tabsHtml}<h3 style="font-size:var(--t-base);margin-bottom:6px">Using Dockge or Arcane? Paste this stack instead.</h3>
  <p class="hint" style="margin:0 0 var(--s4);font-size:var(--t-sm);line-height:1.55">The stack holds only the seed. Quasar’s own services start beside it as separate containers; redeploying or removing the stack never touches them.</p>
  ${opts('gpu-host-6','in 1 hour',true)}
  ${snippet('Stack file',yaml,'Single use · expires at 15:42 · only for gpu-host-6')}
  <div class="note warn" style="margin-top:var(--s4)"><strong>Preparing the host is then your job.</strong> The one-line command checks and fixes the render node, virtual input, user namespaces and the app-container AppArmor profile; a stack cannot. Once the host enrolls, its readiness card lists anything that is missing.</div>`;
 } else if(state==='loading'){
  body=`${tabsHtml}${opts('','in 1 hour')}
  <div class="ae-facts" style="margin-bottom:var(--s4)"><div class="ae-fact"><span>Pinned certificate</span><span class="hint">reading this control plane’s certificate…</span></div></div>
  <button class="btn btn-primary" ${dis}>Create command</button>`;
 } else if(state==='error'){
  body=`${tabsHtml}${opts('gpu-host-2','in 1 hour')}${pinned}
  <div class="note warn" role="alert" style="margin-bottom:var(--s4)"><strong>Could not create the command.</strong> gpu-host-2 is connected right now, so a command bound to its name would be refused. Choose another name, or leave it empty.</div>
  <button class="btn btn-primary">Create command</button>`;
 } else { // http
  body=`${tabsHtml}<div class="note warn"><strong>Open this page over HTTPS to add a host.</strong> From an http:// page the command would tell the new host to connect without TLS, sending its enrollment string and node secret across the network in the clear.</div>`;
 }
 return modal('Add host',body,foot,640);
}

/* ---------------------------------------------------------------------------
   Surface 5 — backup confirmation on a migrating update
   ------------------------------------------------------------------------ */
function rhUpdateModal(state){
 const lead=`<p style="margin:0 0 12px;font-size:var(--t-sm);color:var(--text-2);line-height:1.55">Update the control plane, then 4 eligible hosts, to <b style="color:var(--text)">v${RH.next.v}</b>.</p>
 <p style="margin:0 0 var(--s4);font-size:var(--t-sm);color:var(--text-2);line-height:1.55">This release changes the database. The update waits for every session on the instance to end before it starts, and the control plane restarts: this page loses contact for about a minute. Each host’s sessions end when that host is updated.</p>`;
 const skip=`<div class="note" style="margin-top:var(--s4)">Will be skipped and stay on v${RH.cp.v} (2): study-pc (another owner’s container in the way), gpu-host-5 (offline).</div>`;
 const btn=on=>`<button class="btn btn-ghost">Cancel</button><button class="btn btn-primary"${on?'':' '+dis}>Update</button>`;
 if(state==='own')return modal('Update Quasar',`${lead}
  <div class="note"><strong>Quasar dumps its database first.</strong> Before the control plane on living-room-pc is replaced, its recovery actor dumps Quasar’s database (about 1.4 GB; 212 GB free). If the dump cannot be taken, the update stops there: the control plane is not replaced and the database is not touched. The last three dumps are kept on that machine.</div>${skip}`,btn(true),540);
 if(state==='own-unknown')return modal('Update Quasar',`${lead}
  <div class="note"><strong>Quasar dumps its database first.</strong> Free space on living-room-pc has not been reported, so it is checked just before the dump. If there is not enough, or the dump fails, the update stops there: the control plane is not replaced and the database is not touched.</div>${skip}`,btn(true),540);
 if(state==='own-error')return modal('Update Quasar',`${lead}
  <div class="note warn"><strong>Not enough free space for the database dump.</strong> It needs about 1.4 GB on living-room-pc and 0.6 GB is free. Free some space on that machine, then check again. Without the dump the update cannot start.</div>`,
  `<button class="btn btn-ghost">Cancel</button><button class="btn">${icon('refresh')}Check again</button><button class="btn btn-primary" ${dis}>Update</button>`,540);
 const checked=state==='external-checked';
 return modal('Update Quasar',`${lead}
  <div class="note warn"><strong>Quasar does not back up your database.</strong> It uses the database at <span class="mono">db.example</span> as it is and never dumps, restores or upgrades it. Take a backup with your own tools before you continue: it is the only way back if the update fails.</div>
  <label class="ae-check" style="margin-top:var(--s4);align-items:flex-start"><input type="checkbox"${checked?' checked':''} style="margin-top:2px"><span>I have a current backup of this database, taken after the last change I want to keep.</span></label>
  ${checked?'':`<p class="hint" style="margin:8px 0 0 24px">Update stays unavailable until you confirm.</p>`}${skip}`,btn(checked),540);
}
// The refusal, as the Releases banner reports it after the run.
const rhRefusedBanner=`<div class="card card-pad" style="margin-bottom:var(--s4)">
  <div class="eyebrow" style="color:var(--warning-text)">Update refused</div>
  <div style="font-family:var(--font-display);font-size:1.25rem;font-weight:600;margin-top:7px">v${RH.cp.v} → v${RH.next.v} stopped before the control plane moved</div>
  <p class="hint" style="margin:6px 0 0;line-height:1.55;max-width:80ch;font-size:var(--t-sm)">Quasar could not dump its database on living-room-pc: the disk filled up during the dump. The control plane was not replaced, the database was not touched and no host was updated; sessions can start again. The recovery actor on living-room-pc is already on v${RH.next.v}: it always moves first, and running one release ahead of the control plane on its own machine is expected and harmless. Free some space on that machine and update again.</p>
  ${diag('Details',`attempt: 9e02…7b · control plane machine · outcome: failed\ncomponents: recovery-actor ${RH.cp.v} → ${RH.next.v} (succeeded, kept), control-plane ${RH.cp.v} (not replaced)\nreason: backup_failed (no space left on device)\nschema: 88 (unchanged)`)}</div>`;

/* ---------------------------------------------------------------------------
   Surface 6 — a failed migrating update and its restore command
   ------------------------------------------------------------------------ */
function rhRestore(state){
 // The architecture's shape: the seed image run with `restore --dump <dump>`.
 // The version it returns to is the one the dump was taken under, stated in
 // the copy; whether the command should also name it is open (README).
 const restoreCmd=`docker run --rm -v /var/run/docker.sock:/var/run/docker.sock ${RH.seedImg} restore --dump 2026-09-25T1402Z-schema-88`;
 const head_=`<div class="eyebrow" style="color:var(--danger-text)">Update failed</div>
  <div style="font-family:var(--font-display);font-size:1.25rem;font-weight:600;margin-top:7px">v${RH.next.v} failed after changing the database</div>
  <div class="hint" style="margin-top:4px">Control plane on living-room-pc · 25 Sep 2026, 14:07 · the release’s database migration had already run</div>`;
 const para=t=>`<p style="margin:var(--s4) 0;font-size:var(--t-sm);color:var(--text-2);line-height:1.6;max-width:84ch">${t}</p>`;
 let body;
 if(state==='own')body=`${head_}
  ${para(`Quasar does not undo a migrating update on its own, because the new control plane may already have written data. To go back to <b style="color:var(--text)">v${RH.cp.v}</b>, run this on living-room-pc. It stops the control plane, loads the dump taken at 14:02 — before the migration, under v${RH.cp.v} — into Quasar’s database, and starts v${RH.cp.v} again. Anything written after 14:02 is lost.`)}
  ${snippet('Run on living-room-pc as root',restoreCmd)}
  <p class="hint" style="margin:var(--s3) 0 0;line-height:1.55">The recovery actor on that machine prints the same command in its output, so it can be run while this page is unreachable. The last three dumps are kept.</p>
  ${diag('Attempt details','attempt: 4b90…e1 · control plane · outcome: failed, not restored\nreason: unhealthy (passed a health check at 14:05, failed at 14:07)\ndump: 2026-09-25T1402Z-schema-88 (1.4 GB)\nschema: 88 → 91')}`;
 else if(state==='unknown')body=`${head_}
  ${para(`Quasar does not undo a migrating update on its own. The recovery actor on living-room-pc has not reported which dump it took, so the restore command cannot be shown here yet. The same command is also printed in the recovery actor’s log on that machine.`)}
  <p class="hint" style="margin:var(--s3) 0 0">This card fills in when the recovery actor answers.</p>`;
 else body=`${head_}
  ${para(`Quasar holds no dump of your database, and does not undo a migrating update on its own. To go back to <b style="color:var(--text)">v${RH.cp.v}</b>: restore the backup you confirmed at 13:58 into your database with your own tools, then run this on living-room-pc. It starts v${RH.cp.v} only if the database’s schema matches that release, and refuses otherwise.`)}
  ${snippet('Run on living-room-pc as root, after restoring your backup',`docker run --rm -v /var/run/docker.sock:/var/run/docker.sock ${RH.seedImg} restore --to ${RH.cp.v}`)}
  ${diag('Attempt details','attempt: 4b90…e1 · control plane · outcome: failed, not restored\nreason: unhealthy\ndatabase: external (backup confirmed by admin at 13:58)\nschema: 88 → 91')}`;
 const hist=`<div class="card card-pad"><div class="eyebrow">Apply history</div>
  <div style="margin-top:9px"><div class="rel-fact" style="flex-direction:column;gap:3px">
   <div class="rowflex" style="justify-content:space-between;width:100%"><span style="color:var(--text)">Control plane</span><span class="hint">14:07</span></div>
   <span class="mono" style="font-size:var(--t-xs);color:var(--text-3)">v${RH.cp.v} → v${RH.next.v}</span>
   <span class="hint" style="font-size:var(--t-xs);color:var(--danger-text)">Failed · not restored · ${state==='external'?'your own database':state==='unknown'?'dump not reported yet':'dump kept'}</span></div>
  <div class="rel-fact" style="flex-direction:column;gap:3px">
   <div class="rowflex" style="justify-content:space-between;width:100%"><span style="color:var(--text)">Recovery actor · living-room-pc</span><span class="hint">14:03</span></div>
   <span class="mono" style="font-size:var(--t-xs);color:var(--text-3)">v${RH.cp.v} → v${RH.next.v}</span>
   <span class="hint" style="font-size:var(--t-xs)">Updated · handed over</span></div></div></div>`;
 return `<div class="split" style="grid-template-columns:1fr 300px"><div class="card card-pad">${body}</div>${hist}</div>`;
}

/* ---------------------------------------------------------------------------
   Surface 7 — remove host
   ------------------------------------------------------------------------ */
function rhRemove(state){
 const p=t=>`<p style="margin:0 0 12px;font-size:var(--t-sm);color:var(--text-2);line-height:1.6">${t}</p>`;
 if(state==='confirm')return modal('Remove gpu-host-4?',
  p(`Quasar drains <b style="color:var(--text)">gpu-host-4</b> — it takes no new sessions and waits for its <b style="color:var(--text)">2 live sessions</b> to end — then stops and removes its node agent and recovery actor.`)+
  p('Its homes and data stay on the machine. The seed stays too, idle; remove it the way you started it, when you like. To bring the machine back, add a host with the same node name: its history is kept.'),
  `<button class="btn btn-ghost">Cancel</button><button class="btn btn-danger">${icon('trash')}Remove host</button>`);
 if(state==='offline')return modal('Remove gpu-host-5?',
  `<div class="note warn" style="margin-bottom:12px"><strong>gpu-host-5 is not connected</strong> (last seen 3 days ago). Removing a host needs it connected, so its recovery actor can stop and remove the containers.</div>`+
  p('Reconnect it and try again. If the machine is gone, you can leave it: an offline host takes no sessions, and a new machine can be added under the same node name.'),
  `<button class="btn btn-ghost">Close</button><button class="btn btn-danger" ${dis}>${icon('trash')}Remove host</button>`);
 const h=rhHost('gpu-host-4');
 const sub={agentSub:'older than the control plane'};
 if(state==='progress')return `<div class="page">${rhDetailHead(h,chip('removing','info'))}
  <div class="note" style="margin-bottom:var(--s4)"><strong>Removing gpu-host-4.</strong> It takes no new sessions and is waiting for 2 live sessions to end (longest: 41 min so far). Its node agent and recovery actor are removed after that.</div>
  ${rhServices(h,{...sub,foot:`<p class="hint" style="margin:0;flex:1">Removal started by salty2011 at 14:12.</p><button class="btn btn-sm">Cancel removal</button>`})}</div>`;
 return `<div class="page">${rhDetailHead(h,chip('drained','warning'))}
  <div class="note warn" style="margin-bottom:var(--s4);display:flex;gap:var(--s5);align-items:center;flex-wrap:wrap"><div style="flex:1;min-width:280px"><strong>Removing gpu-host-4 did not finish.</strong> The host is drained and still enrolled. Its recovery actor could not remove the node agent; nothing else changed.${diag('Details','attempt: 22fd…09 · remove · outcome: failed\nreason: engine error removing the node agent container: device or resource busy')}</div><button class="btn btn-danger btn-sm">${icon('refresh')}Retry removal</button></div>
  ${rhServices(h,{...sub,foot:false})}</div>`;
}

/* ---------------------------------------------------------------------------
   Surface 8 — developer apply (the drawer pattern, as pages-library.js opens it)
   ------------------------------------------------------------------------ */
function rhDevApply(state){
 const x=`<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M4 4l8 8M12 4l-8 8" stroke-linecap="round"/></svg>`;
 const ra='ghcr.io/accreleus/quasar-recovery@sha256:9b3e…41d7',na='ghcr.io/accreleus/quasar-node-agent@sha256:c05a…8e22',ca='ghcr.io/accreleus/quasar-control-plane@sha256:e71f…0b3c';
 // [recovery actor, node agent, control plane]; null = not on this machine.
 const val={
  filled:[ra,na,null],
  empty:['','',null],
  error:['registry.example.org/team/quasar-recovery@sha256:9b3e…41d7','ghcr.io/accreleus/quasar-node-agent:my-branch',null],
  migrating:[ra,'',ca],
  'migrating-external':[ra,null,ca]}[state];
 const err=[state==='error'?'registry.example.org/team is not an allowed namespace.':'',state==='error'?'Use a digest (@sha256:…), not a tag.':''];
 const f=(label,v,e,ph,off)=>`<div class="field"><label class="label">${label}</label><input class="input mono" value="${v}" placeholder="${ph}"${e?' style="border-color:var(--danger)" aria-invalid="true"':''}${off?' '+dis:''}>${e?`<span class="hint" style="color:var(--danger-text)">${e}</span>`:''}</div>`;
 const img=(label,v,e,ph)=>v===null?f(label,'','','not on this machine',true):f(label,v,e,ph);
 const mig=state.startsWith('migrating'),ext=state==='migrating-external';
 const target=ext?'<option>attic-server · Control-only host</option><option>gpu-host-2 · GPU host</option>'
  :mig?'<option>living-room-pc · Combined host</option><option>gpu-host-2 · GPU host</option>'
  :'<option>gpu-host-2 · GPU host</option><option>living-room-pc · Combined host</option>';
 // A migrating control-plane digest follows the same database rule as a
 // migrating release (Decision 14): never unattended, drain first, and a dump
 // of Quasar's own database or the operator's confirmed backup of theirs.
 const dbSec=!mig?'':`<div class="fsec"><div class="fs-label"><h4>Database</h4><p>This control-plane image changes the database (schema 88 → 91).</p></div>
   <div class="fs-fields">
    <p class="hint" style="margin:0;font-size:var(--t-sm);line-height:1.55;color:var(--text-2)">Like a migrating release, this apply waits for every session on the instance to end before the control plane is replaced, and is never applied unattended.</p>
    ${ext?`<div class="note warn"><strong>Quasar does not back up your database.</strong> It uses the database at <span class="mono">db.example</span> as it is and never dumps, restores or upgrades it. Take a backup with your own tools first: it is the only way back if this build fails.</div>
    <label class="ae-check" style="align-items:flex-start"><input type="checkbox" style="margin-top:2px"><span>I have a current backup of this database, taken after the last change I want to keep.</span></label>`
    :`<div class="note"><strong>Quasar dumps its database first.</strong> Before the control plane on living-room-pc is replaced, its recovery actor dumps Quasar’s database (about 1.4 GB; 212 GB free). If the dump cannot be taken, the apply stops there: the control plane is not replaced and the database is not touched.</div>`}
   </div></div>`;
 const ok=state==='filled'||state==='migrating';
 const footHint=ok?'Checks each digest at the registry before anything stops.':state==='empty'?'Enter at least one image by digest.':ext?'Confirm your backup to continue.':'Fix the two images above to continue.';
 return `<aside class="drawer">
 <div class="dw-head"><div><div class="eyebrow mono" style="text-transform:none;letter-spacing:0">developer lane · not a release</div><h2 style="margin-top:4px">Developer apply</h2></div><button class="icon-btn" style="margin-left:auto" aria-label="Close">${x}</button></div>
 <div class="dw-body">
  <div class="note warn" style="margin-bottom:var(--s5)"><strong>For testing a build that is not a release.</strong> Applies images by digest to one Quasar-owned machine. It is recorded as an attempt like any other, the recovery actor moves first, and a failed agent is put back automatically. Applying a node agent ends that host’s sessions.</div>
  <div class="fsec"><div class="fs-label"><h4>Target</h4><p>One machine at a time. Machines built from source are not offered.</p></div>
   <div class="fs-fields"><div class="field"><label class="label">Machine</label><select class="select">${target}<option disabled>workbench · built from source</option></select></div></div></div>
  <div class="fsec"><div class="fs-label"><h4>Images</h4><p>Leave a service empty to keep what it runs.</p></div>
   <div class="fs-fields">
    ${img('Recovery actor',val[0],err[0],'namespace/quasar-recovery@sha256:…')}
    ${img('Node agent',val[1],err[1],'namespace/quasar-node-agent@sha256:…')}
    ${img('Control plane',val[2],'','namespace/quasar-control-plane@sha256:…')}
   </div></div>
  ${dbSec}
  <div class="fsec"><div class="fs-label"><h4>Allowed namespaces</h4><p>Set on each machine. Images from anywhere else are refused.</p></div>
   <div class="fs-fields"><div class="ae-facts">${RH.ns.map(n=>`<div class="ae-fact"><span class="mono" style="color:var(--text-2)">${n}</span><span class="hint">allowed</span></div>`).join('')}</div></div></div>
 </div>
 <div class="dw-foot"><span class="hint">${footHint}</span>
  <div style="margin-left:auto;display:flex;gap:8px"><button class="btn btn-ghost">Cancel</button><button class="btn btn-primary"${ok?'':' '+dis}>Apply digests</button></div></div>
 </aside>`;
}

Object.assign(window,{RH,RH_HOSTS,pageRhHosts,pageRhInventory,pageRhWarning,pageRhFloor,pageRhReleases,rhInstalled,rhTargets,rhDevCard,rhAddHost,rhUpdateModal,rhRefusedBanner,rhRestore,rhRemove,rhDevApply,stage});
