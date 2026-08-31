// Overview · Sessions · Session detail
const D=window.QDATA;
function kpi(k,v,unit,meta,trend,go){
 return `<button class="card card-pad kpi" onclick="go('${go}')" style="text-align:left;cursor:pointer;display:block;width:100%">
 <div class="eyebrow">${k}</div>
 <div style="display:flex;align-items:flex-end;gap:10px;margin-top:9px">
  <div style="font-family:var(--font-display);font-size:2rem;font-weight:600;line-height:1">${v}<span style="font-size:.95rem;color:var(--text-2);font-weight:500;margin-left:4px">${unit||''}</span></div>
  <div style="margin-left:auto;margin-bottom:3px">${trend||''}</div>
 </div>
 <div style="font-size:var(--t-sm);color:var(--text-3);margin-top:7px">${meta}</div></button>`;
}
function alertRow(a){
 const c=a.sev==='err'?'danger':'warning';
 return `<div style="display:flex;gap:12px;padding:13px var(--card-pad);border-bottom:1px solid var(--line);align-items:flex-start">
 <span class="alert-ico" style="width:26px;height:26px;border-radius:50%;flex:none;display:grid;place-content:center;background:var(--${c}-bg);color:var(--${c});margin-top:1px">${icon('alert')}</span>
 <div style="min-width:0;flex:1">
  <div style="font-size:var(--t-sm);font-weight:600;color:var(--text)">${esc(a.title)}</div>
  <div style="font-size:var(--t-xs);color:var(--text-3);font-family:var(--font-mono);margin-top:3px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis">${esc(a.body)}</div>
 </div>
 <div style="display:flex;align-items:center;gap:10px;flex:none">
  <span style="font-size:var(--t-xs);color:var(--text-4);font-family:var(--font-mono)">${a.age}</span>
  <button class="btn btn-sm" onclick="go('${a.go}')">${a.cta}</button>
 </div></div>`;
}
function pageOverview(){
 const live=D.SESSIONS.filter(s=>s.state==='streaming'||s.state==='degraded'||s.state==='connecting');
 const online=D.HOSTS.filter(h=>h.state==='online').length;
 const bad=D.HOSTS.filter(h=>h.state==='degraded'||h.state==='offline').length;
 const slotsUsed=D.HOSTS.reduce((n,h)=>n+h.gpus.reduce((m,g)=>m+g.slots[0],0),0);
 const slotsTotal=D.HOSTS.reduce((n,h)=>n+h.gpus.reduce((m,g)=>m+g.slots[1],0),0);
 const bw=live.reduce((n,s)=>n+s.br,0).toFixed(1);
 const hostRow=h=>{
  const v=h.gpus.reduce((a,g)=>a+g.vram[0],0),vt=h.gpus.reduce((a,g)=>a+g.vram[1],0);
  const su=h.gpus.reduce((a,g)=>a+g.slots[0],0),st=h.gpus.reduce((a,g)=>a+g.slots[1],0);
  const p=pct(su,st);
  return `<tr class="clickable" onclick="go('#/fleet/hosts/${h.id}')">
   <td><div class="rowflex"><span class="primary">${h.name}</span>${chip(h.state)}</div><div class="sub" style="margin-top:1px">${h.gpus.map(g=>g.n).join(' · ')}</div></td>
   <td style="width:180px"><div class="bar-row"><span>SLOTS</span>${bar(su,st,tone(p))}<span class="v">${su}/${st}</span></div>
   <div class="bar-row" style="margin-top:5px"><span>VRAM</span>${bar(v,vt,tone(pct(v,vt)))}<span class="v">${Math.round(v)}/${vt}G</span></div></td>
   <td class="right num" style="width:70px">${h.sessions}</td>
   <td class="right" style="width:90px"><span class="num" style="color:${h.state==='online'?'var(--success-text)':'var(--text-3)'}">${h.hb}</span></td></tr>`;
 };
 return `<div class="page">
 ${head('Overview','Live fleet state · updated 3 seconds ago',
  `<div class="segmented"><button aria-selected="true">1h</button><button aria-selected="false">24h</button><button aria-selected="false">7d</button></div>
   <button class="btn btn-ghost">${icon('refresh')}Refresh</button>`)}
 <div class="grid g4" style="margin-bottom:var(--s5)">
  ${kpi('Live sessions',live.length,'',`${live.filter(s=>s.state==='degraded').length} degraded · ${bw} Mb/s out`,spark([6,7,9,8,11,10,12,11,13,live.length],'var(--success)'),'#/sessions')}
  ${kpi('GPU slots',slotsUsed,`/ ${slotsTotal}`,`${slotsTotal-slotsUsed} free across ${online} online hosts`,spark([4,5,5,6,7,6,8,7,7,slotsUsed],'var(--accent)'),'#/fleet/hosts')}
  ${kpi('Hosts',online,`/ ${D.HOSTS.length}`,bad?`<span style="color:var(--danger-text)">${bad} need attention</span>`:'all healthy',spark([6,6,6,5,5,6,5,4,4,online],'var(--danger)'),'#/fleet/hosts')}
  ${kpi('Users',D.USERS.filter(u=>u.state==='active').length,'active',`${D.USERS.filter(u=>u.last==='streaming').length} streaming now · 2 invites pending`,spark([7,7,8,8,8,8,8,8,8,8],'var(--info)'),'#/people/users')}
 </div>
 <div class="split even" style="margin-bottom:var(--s5)">
  <div class="card">
   <div class="panel-head"><span class="live-dot"></span><span class="panel-title">Live sessions</span>
    <div class="acts"><button class="btn btn-sm btn-ghost" onclick="go('#/sessions')">All sessions ${icon('chev')}</button></div></div>
   <div class="table-wrap"><table class="qtable"><thead><tr><th>Session</th><th>Host</th><th class="right">FPS</th><th>Trend</th><th class="right">Latency</th><th class="right">Bitrate</th></tr></thead><tbody>
   ${live.map(s=>`<tr class="clickable" onclick="go('#/sessions/${s.id}')">
    <td><div class="stack"><span class="primary">${esc(s.app)}</span><span class="sub">${s.user} · ${s.dur}</span></div></td>
    <td><div class="stack"><span>${s.host.replace('quasar-','')}</span><span class="sub">${s.gpu}</span></div></td>
    <td class="right num" style="color:${s.state==='degraded'?'var(--warning-text)':'var(--text)'}">${s.fps||'—'}</td>
    <td style="width:84px">${spark(s.fpsT,s.state==='degraded'?'var(--warning)':'var(--success)')}</td>
    <td class="right num" style="color:${s.lat>50?'var(--danger-text)':'var(--text-2)'}">${s.lat?s.lat+' ms':'—'}</td>
    <td class="right num">${s.br?s.br.toFixed(1):'—'}</td></tr>`).join('')}
   </tbody></table></div></div>
  <div class="card">
   <div class="panel-head"><span class="panel-title">Needs attention</span><div class="acts"><span class="chip chip-danger">${D.ALERTS.filter(a=>a.sev==='err').length} critical</span><span class="chip chip-warning">${D.ALERTS.filter(a=>a.sev==='warn').length} warning</span></div></div>
   ${D.ALERTS.map(alertRow).join('')}
   <div style="padding:11px var(--card-pad);margin-top:auto"><button class="btn btn-sm btn-ghost" onclick="go('#/audit')">Open audit log ${icon('chev')}</button></div></div>
 </div>
 <div class="split even">
  <div class="card">
   <div class="panel-head"><span class="panel-title">Fleet capacity</span><div class="acts"><span class="chip">${slotsUsed}/${slotsTotal} slots</span><button class="btn btn-sm btn-ghost" onclick="go('#/fleet/hosts')">Hosts ${icon('chev')}</button></div></div>
   <div class="table-wrap"><table class="qtable"><thead><tr><th>Host</th><th>Capacity</th><th class="right">Sessions</th><th class="right">Heartbeat</th></tr></thead><tbody>${D.HOSTS.map(hostRow).join('')}</tbody></table></div></div>
  <div class="card">
   <div class="panel-head"><span class="panel-title">Recent activity</span><div class="acts"><button class="btn btn-sm btn-ghost" onclick="go('#/audit')">View all</button></div></div>
   <div style="padding:6px 0">${D.AUDIT.slice(0,6).map(a=>`<div style="display:flex;gap:11px;padding:9px var(--card-pad);align-items:baseline">
    <span class="num" style="font-size:var(--t-xs);color:var(--text-4);flex:none">${a.t}</span>
    <div style="min-width:0"><div style="font-size:var(--t-sm)"><span style="color:var(--text);font-weight:600">${a.actor}</span> <span class="mono" style="font-size:var(--t-xs);color:${a.sev==='err'?'var(--danger-text)':a.sev==='warn'?'var(--warning-text)':'var(--text-3)'}">${a.action}</span> ${esc(a.target)}</div></div></div>`).join('')}</div></div>
 </div></div>`;
}
function pageSessions(){
 const counts=D.SESSIONS.reduce((m,s)=>(m[s.state]=(m[s.state]||0)+1,m),{});
 return `<div class="page">
 ${head('Sessions','Every stream on the fleet, live and recent',`<button class="btn btn-ghost">${icon('refresh')}Refresh</button>`)}
 <div class="toolbar">
  <div class="segmented"><button aria-selected="true">All <span class="num" style="opacity:.7">${D.SESSIONS.length}</span></button><button aria-selected="false">Live <span class="num" style="opacity:.7">${(counts.streaming||0)+(counts.degraded||0)}</span></button><button aria-selected="false">Failed <span class="num" style="opacity:.7">${counts.failed||0}</span></button></div>
  <div class="search">${icon('search')}<input placeholder="Filter by user, app or host"></div>
  <select class="select"><option>All hosts</option>${D.HOSTS.map(h=>`<option>${h.name}</option>`).join('')}</select>
  <div class="right"><span style="font-size:var(--t-xs);color:var(--text-3)">Auto-refresh</span><span class="switch" aria-checked="true" role="switch"></span></div>
 </div>
 ${tableCard([{l:'Session'},{l:'User'},{l:'Host / GPU'},{l:'FPS',a:'right'},{l:'Trend'},{l:'Latency',a:'right'},{l:'Bitrate',a:'right'},{l:'Codec'},{l:'Duration',a:'right'},{l:'',a:'right'}],
  D.SESSIONS.map(s=>`<tr class="clickable" onclick="go('#/sessions/${s.id}')">
   <td><div class="rowflex">${sdot(s.state,s.state==='degraded'?'warning':null)}<div class="stack"><span class="primary">${esc(s.app)}</span><span class="sub mono">${s.id}${s.state!=='streaming'?' · '+s.state:''}</span></div></div></td>
   <td>${s.user}</td>
   <td><div class="stack"><span>${s.host.replace('quasar-','')}</span><span class="sub">${s.gpu}</span></div></td>
   <td class="right num" style="color:${s.state==='degraded'?'var(--warning-text)':'var(--text-2)'}">${s.fps||'—'}</td>
   <td style="width:84px">${s.fps?spark(s.fpsT,s.state==='degraded'?'var(--warning)':'var(--success)'):''}</td>
   <td class="right num" style="color:${s.lat>50?'var(--danger-text)':'var(--text-2)'}">${s.lat?s.lat+' ms':'—'}</td>
   <td class="right num">${s.br?s.br.toFixed(1)+' Mb':'—'}</td>
   <td><span class="cell-id">${s.codec}</span></td>
   <td class="right num">${s.dur}</td>
   <td class="cell-actions"><button class="icon-btn" onclick="event.stopPropagation()">${icon('dots')}</button></td></tr>`).join(''))}
 </div>`;
}
function chartCard(title,unit,series,color,val){
 const w=520,h=110,max=Math.max(...series)*1.15||1;
 const pts=series.map((v,i)=>`${(i/(series.length-1)*w).toFixed(1)},${(h-(v/max)*h).toFixed(1)}`).join(' ');
 const area=`0,${h} ${pts} ${w},${h}`;
 return `<div class="card card-pad">
  <div style="display:flex;align-items:baseline;gap:10px"><span class="eyebrow">${title}</span><span class="num" style="margin-left:auto;font-size:var(--t-lg);color:var(--text)">${val}<span style="font-size:var(--t-xs);color:var(--text-3);margin-left:3px">${unit}</span></span></div>
  <svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" style="width:100%;height:88px;margin-top:10px;overflow:visible">
   <defs><linearGradient id="g${title.replace(/\W/g,'')}" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="${color}" stop-opacity=".26"/><stop offset="1" stop-color="${color}" stop-opacity="0"/></linearGradient></defs>
   ${[0,.25,.5,.75,1].map(f=>`<line x1="0" y1="${(h*f).toFixed(0)}" x2="${w}" y2="${(h*f).toFixed(0)}" stroke="var(--line)" stroke-width="1"/>`).join('')}
   <polygon points="${area}" fill="url(#g${title.replace(/\W/g,'')})"/>
   <polyline points="${pts}" fill="none" stroke="${color}" stroke-width="1.8" stroke-linejoin="round" stroke-linecap="round"/></svg></div>`;
}
function pageSessionDetail(id){
 const s=D.SESSIONS.find(x=>x.id===id)||D.SESSIONS[0];
 const seq=n=>Array.from({length:40},(_,i)=>n*(.86+Math.sin(i/3.1+n)*.07+Math.random()*.06));
 return `<div class="page">
 <div class="crumbs"><a onclick="go('#/sessions')">Sessions</a>${icon('chev')}<span class="mono">${s.id}</span></div>
 <div class="card" style="margin-bottom:var(--s4)">
  <div class="page-head" style="margin:0;padding:var(--card-pad) var(--card-pad) var(--s4)">
   <div>
    <div class="rowflex shdr-title">${sdot(s.state,s.state==='degraded'?'warning':null)}<h1>${esc(s.app)}</h1></div>
    <div class="sub" style="margin-top:5px">${s.user} · ${s.host} · ${s.gpu} · ${s.state}</div>
   </div>
   <div class="acts"><button class="btn btn-ghost">${icon('download')}Export trace</button>${s.state==='ended'||s.state==='failed'?'':'<button class="btn btn-danger">Terminate</button>'}</div>
  </div>
  <div class="six" style="padding:var(--s4) var(--card-pad) var(--card-pad);border-top:1px solid var(--line)">
   ${[['Resolution',s.res],['Codec',s.codec],['Frame rate',(s.fps||0)+' fps'],['Latency',(s.lat||0)+' ms'],['Bitrate',(s.br||0)+' Mb/s'],['Duration',s.dur]].map(([k,v])=>`<div><div class="eyebrow">${k}</div><div class="num" style="font-size:var(--t-lg);color:var(--text);margin-top:5px">${v}</div></div>`).join('')}
  </div>
 </div>
 <div class="grid g2" style="margin-bottom:var(--s4)">
  ${chartCard('Frames per second','fps',seq(s.fps||60),'var(--success)',s.fps||0)}
  ${chartCard('Round-trip latency','ms',seq(s.lat||14),'var(--info)',s.lat||0)}
  ${chartCard('Bitrate','Mb/s',seq(s.br||24),'var(--accent)',s.br||0)}
  ${chartCard('Frame time','ms',seq(8.4),'var(--lavender)','8.4')}
 </div>
 <div class="grid g2">
  <div class="card"><div class="panel-head"><span class="panel-title">Agent latest</span><span class="hint" style="margin-left:auto">2s ago</span></div>
   <div class="table-wrap"><table class="qtable">${[['encoder','NVENC AV1'],['capture','KMS/DRM'],['queue depth','1'],['dropped frames','0'],['encode time','3.1 ms']].map(([k,v])=>`<tr><td style="color:var(--text-3)">${k}</td><td class="right num primary">${v}</td></tr>`).join('')}</table></div></div>
  <div class="card"><div class="panel-head"><span class="panel-title">Browser latest</span><span class="hint" style="margin-left:auto">2s ago</span></div>
   <div class="table-wrap"><table class="qtable">${[['decoder','hardware AV1'],['jitter buffer','12 ms'],['packets lost','0.02%'],['nack / pli','3 / 0'],['decode time','2.4 ms']].map(([k,v])=>`<tr><td style="color:var(--text-3)">${k}</td><td class="right num primary">${v}</td></tr>`).join('')}</table></div></div>
 </div></div>`;
}
Object.assign(window,{pageOverview,pageSessions,pageSessionDetail,chartCard});
