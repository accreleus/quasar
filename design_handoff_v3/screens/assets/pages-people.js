// People: Users · Invites. Audit log. Settings.
const P=window.QDATA;
const PPL_TABS=n=>tabs([
 {id:'users',label:'Users',count:P.USERS.length,go:'#/people/users'},
 {id:'invites',label:'Invites',count:P.INVITES.filter(i=>i.state==='pending').length,go:'#/people/invites'}],n);
function pageUsers(){
 return `<div class="page">
 ${head('People',`${P.USERS.filter(u=>u.state==='active').length} active · ${P.USERS.filter(u=>u.role==='Admin').length} admins`,`<button class="btn btn-ghost">${icon('download')}Export</button><button class="btn btn-primary">${icon('plus')}Invite user</button>`)}
 ${PPL_TABS('users')}
 <div class="toolbar">
  <div class="segmented"><button aria-selected="true">All</button><button aria-selected="false">Admins</button><button aria-selected="false">Disabled <span class="num" style="opacity:.7">1</span></button></div>
  <div class="search">${icon('search')}<input placeholder="Filter by name or email"></div>
 </div>
 <div class="card" style="margin-bottom:var(--s3);padding:9px var(--card-pad);display:flex;align-items:center;gap:var(--s4);border-color:var(--accent-soft-2);background:var(--accent-soft)">
  <span style="font-size:var(--t-sm);font-weight:600">2 selected</span>
  <div style="display:flex;gap:8px;margin-left:auto"><button class="btn btn-sm">Change role</button><button class="btn btn-sm">Disable</button><button class="btn btn-sm btn-danger">Delete</button></div>
 </div>
 ${tableCard([{l:'',w:'34px'},{l:'User'},{l:'Role'},{l:'State'},{l:'Sessions',a:'right'},{l:'Home size',a:'right'},{l:'Last seen',a:'right'},{l:''}],
  P.USERS.map((u,i)=>`<tr class="clickable" style="${u.state==='disabled'?'opacity:.6':''}">
   <td><span style="display:inline-block;width:14px;height:14px;border-radius:4px;border:1px solid ${i<2?'var(--accent)':'var(--line-3)'};background:${i<2?'var(--accent)':'transparent'}"></span></td>
   <td><div class="rowflex"><span class="u-ava">${u.name[0].toUpperCase()}</span><div class="stack"><span class="primary">${u.name}</span><span class="sub">${u.mail}</span></div></div></td>
   <td>${chip(u.role,u.role==='Admin'?'accent':'neutral')}</td>
   <td>${u.last==='streaming'?chip('streaming'):chip(u.state)}</td>
   <td class="right num">${u.sessions}</td>
   <td class="right num">${u.home}</td>
   <td class="right" style="color:var(--text-3)">${u.last}</td>
   <td class="cell-actions"><button class="icon-btn">${icon('dots')}</button></td></tr>`).join(''))}
 </div>`;
}
function pageInvites(){
 return `<div class="page">
 ${head('People','Invite-gated registration',`<button class="btn btn-primary">${icon('plus')}Mint invite</button>`)}
 ${PPL_TABS('invites')}
 <div class="card card-pad" style="margin-bottom:var(--s4);display:flex;gap:var(--s7);align-items:center;flex-wrap:wrap">
  <div><div class="eyebrow">Registration mode</div>
   <div class="segmented" style="margin-top:8px"><button aria-selected="false">Closed</button><button aria-selected="true">Invite only</button><button aria-selected="false">Open</button></div></div>
  <div class="note warn" style="max-width:460px;margin-left:auto">An invite code is shown <strong>once</strong>, when it is minted. It cannot be retrieved afterwards — revoke and mint a new one instead.</div>
 </div>
 ${tableCard([{l:'Code'},{l:'State'},{l:'Created by'},{l:'Created',a:'right'},{l:'Expires',a:'right'},{l:'Uses',a:'right'},{l:''}],
  P.INVITES.map(v=>`<tr style="${v.state==='pending'?'':'opacity:.65'}">
   <td><span class="cell-id" style="font-size:var(--t-sm)">${v.code}</span></td>
   <td>${chip(v.state)}</td>
   <td>${v.by}</td>
   <td class="right" style="color:var(--text-3)">${v.created}</td>
   <td class="right" style="color:${v.expires==='expired'?'var(--danger-text)':'var(--text-3)'}">${v.expires}</td>
   <td class="right num">${v.uses}</td>
   <td class="cell-actions">${v.state==='pending'?'<button class="btn btn-sm btn-ghost">Copy link</button><button class="btn btn-sm btn-danger">Revoke</button>':'<button class="icon-btn">'+icon('dots')+'</button>'}</td></tr>`).join(''))}
 </div>`;
}
function pageAudit(){
 const sevC={err:'danger',warn:'warning',info:'neutral'};
 const detailFull={
  'host.drain':'reason=maintenance window 14:00–16:00\nrequested_by=salty2011\ndrain_mode=graceful\nsessions_migrated=1 sessions_ended=0\ncordon=true\nexpected_return=2026-08-08T16:00:00Z',
  'session.failed':'encoder_init_failed: vaapi device busy\ndevice=/dev/dri/renderD128 pid=48211\ncodec=hevc profile=main10 bitrate=31400000\nretry_attempts=3 backoff_ms=250,500,1000\nagent_version=0.1.0 host=quasar-node-3\ntrace_id=8f31c0a2-77bd-4e91-9c2f-0d51ab7e3311',
  'app.updated':'launch_profile: Adaptive 1080p → Adaptive 1440p\nchanged_by=priya\nprevious_rungs=[hevc-1080p60, h264-1080p60]\nnew_rungs=[av1-1440p60, hevc-1440p60, h264-1080p60]\naffected_sessions=0 (applies to new sessions only)',
  'invite.created':'expires_in=7d max_uses=1\ncode_hash=sha256:4b1e…a2f9\nissued_by=salty2011\nregistration_mode=invite_only',
  'host.degraded':'heartbeat late 41s; gpu enumeration returned 0 devices\nlast_ok=2026-08-08T12:56:32Z\nnvml_error=NVML_ERROR_DRIVER_NOT_LOADED\nscheduling=paused\nsessions_affected=0',
  'session.started':'app=Blender host=quasar-node-1 tier=workstation-4k\ncodec=av1 resolution=3840x2160 fps=60\nrender_node=/dev/dri/renderD128 slot=1/3\nclient=chrome/127 os=macOS 15.2',
  'user.role_changed':'role: user → disabled\nchanged_by=priya\nsessions_revoked=2\nhome_retained=true (11 GB)',
  'settings.updated':'registration_mode: closed → invite_only\nchanged_by=salty2011\nprevious=closed\ninvites_outstanding=2'};
 return `<div class="page">
 ${head('Audit log','Who changed what, and what the system did about it',`<button class="btn btn-ghost">${icon('download')}Export CSV</button>`)}
 <div class="toolbar">
  <div class="segmented"><button aria-selected="true">All</button><button aria-selected="false">Operator</button><button aria-selected="false">System</button><button aria-selected="false">Errors <span class="num" style="opacity:.7">2</span></button></div>
  <div class="search">${icon('search')}<input placeholder="Filter by actor, action or target"></div>
  <select class="select"><option>Last 24 hours</option><option>Last 7 days</option><option>Last 30 days</option></select>
  <div class="right"><button class="btn btn-ghost btn-sm" onclick="toggleAllAudit()">Expand all</button></div>
 </div>
 <div class="card"><div class="panel-head"><span class="eyebrow">Today · 8 August 2026</span><div class="acts"><span class="chip">8 entries</span></div></div>
 <div class="table-wrap"><table class="qtable audit"><thead><tr><th style="width:36px"></th><th style="width:88px">Time</th><th style="width:150px">Actor</th><th style="width:190px">Action</th><th style="width:200px">Target</th><th>Detail</th><th style="width:1%"></th></tr></thead><tbody>
 ${P.AUDIT.map((a,i)=>`<tr class="clickable" onclick="toggleAudit(${i})">
  <td><span class="aud-caret" id="ac${i}">${icon('chev')}</span></td>
  <td class="num" style="color:var(--text-3)">${a.t}</td>
  <td><div class="rowflex"><span class="u-ava" style="width:20px;height:20px;font-size:9px;${a.actor==='system'?'background:var(--ink-5);color:var(--text-3)':''}">${a.actor==='system'?'S':a.actor[0].toUpperCase()}</span><span class="primary">${a.actor}</span></div></td>
  <td><span class="chip${sevC[a.sev]==='neutral'?'':' chip-'+sevC[a.sev]}" style="font-family:var(--font-mono);text-transform:none;letter-spacing:0">${a.action}</span></td>
  <td class="primary">${esc(a.target)}</td>
  <td class="mono" style="font-size:var(--t-xs);color:var(--text-3);white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:0">${esc(a.detail)}</td>
  <td class="cell-actions"><button class="icon-btn" title="Copy entry" onclick="event.stopPropagation();copyAudit(${i},this)">${icon('copy')}</button></td></tr>
 <tr class="aud-detail" id="ad${i}" hidden><td></td><td colspan="6" class="aud-cell">
   <div class="aud-det">
    <div class="rowflex aud-det-head"><span class="eyebrow">Detail</span>
     <button class="btn btn-sm btn-ghost" style="margin-left:auto" onclick="event.stopPropagation();copyAudit(${i},this)">${icon('copy')}Copy</button></div>
    <pre id="ap${i}" class="aud-pre">${esc(detailFull[a.action]||a.detail)}</pre>
   </div></td></tr>`).join('')}
 </tbody></table></div></div></div>`;
}
function toggleAudit(i){
 const r=document.getElementById('ad'+i),c=document.getElementById('ac'+i);
 if(!r)return;r.hidden=!r.hidden;c.classList.toggle('open',!r.hidden);
}
function toggleAllAudit(){
 const rows=document.querySelectorAll('.aud-detail');const anyClosed=[...rows].some(r=>r.hidden);
 rows.forEach((r,i)=>{r.hidden=!anyClosed;const c=document.getElementById('ac'+i);if(c)c.classList.toggle('open',anyClosed);});
 const b=event&&event.currentTarget;if(b)b.textContent=anyClosed?'Collapse all':'Expand all';
}
function copyAudit(i,btn){
 const pre=document.getElementById('ap'+i);if(!pre)return;
 const a=P.AUDIT[i];
 const text=`${a.t}  ${a.actor}  ${a.action}  ${a.target}\n${pre.textContent}`;
 navigator.clipboard&&navigator.clipboard.writeText(text);
 const old=btn.innerHTML;btn.innerHTML=btn.classList.contains('btn')?'Copied':icon('check');
 setTimeout(()=>{btn.innerHTML=old},1200);
}
function pageSettings(){
 const sec=(title,desc,body)=>`<div class="card" style="margin-bottom:var(--s4)"><div class="panel-head"><div><span class="panel-title">${title}</span><div class="hint" style="margin-top:3px">${desc}</div></div></div><div class="card-pad">${body}</div></div>`;
 const row=(l,h,ctrl)=>`<div class="rowflex" style="justify-content:space-between;gap:var(--s6);padding:11px 0;border-bottom:1px solid var(--line)"><div><div class="label">${l}</div><div class="hint">${h}</div></div><div style="flex:none">${ctrl}</div></div>`;
 return `<div class="page" style="max-width:880px">
 ${head('Settings','Instance-wide configuration')}
 ${sec('Instance','Identity and access for this Quasar deployment',
  `${row('Instance name','Shown on the login page and in invite emails','<input class="input" value="studio.io gaming" style="width:240px">')}
   ${row('Public URL','Where clients connect','<input class="input mono" value="https://play.studio.io" style="width:240px">')}
   ${row('Registration','Who can create an account','<div class="segmented"><button aria-selected="false">Closed</button><button aria-selected="true">Invite only</button><button aria-selected="false">Open</button></div>')}`)}
 ${sec('Scheduling','How sessions are placed on hosts',
  `${row('Placement strategy','Which host a new session lands on','<select class="select"><option>Least loaded GPU</option><option>Pack onto fewest hosts</option><option>Round robin</option></select>')}
   ${row('Session slot limit','Maximum concurrent sessions per GPU','<input class="input num" value="3" style="width:80px">')}
   ${row('Idle timeout','Disconnect a session after inactivity','<input class="input num" value="30 min" style="width:110px">')}`)}
 ${sec('Appearance','Applies to your account only',
  `${row('Theme','Dark is the default','<div class="segmented"><button aria-selected="true" onclick="setTheme(\'dark\')">Dark</button><button aria-selected="false" onclick="setTheme(\'light\')">Light</button></div>')}
   ${row('Density','Row heights and paddings across the console','<div class="segmented"><button aria-selected="true" onclick="setDensity(\'comfortable\')">Comfortable</button><button aria-selected="false" onclick="setDensity(\'dense\')">Dense</button></div>')}`)}
 ${sec('Danger zone','Irreversible operations',
  `<div class="rowflex" style="justify-content:space-between"><div><div class="label">Rotate agent enrollment token</div><div class="hint">All hosts must re-enroll with the new token</div></div><button class="btn btn-danger">Rotate token</button></div>`)}
 </div>`;
}
Object.assign(window,{pageUsers,pageInvites,pageAudit,pageSettings,toggleAudit,toggleAllAudit,copyAudit});
