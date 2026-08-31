// /account/* — the signed-in user's own account, modelled on the codebase's
// AccountLayout + accountNav.ts: its own left sub-nav (grouped by subject),
// replacing the console rail, with the shared topbar kept.
const AC=window.QDATA;
const ME={name:'salty2011',mail:'admin@quasar.local',role:'Admin',joined:'08/14/2025',last:'now',home:'12.4 GB'};
const ACCOUNT_SECTIONS=[
 {id:'account',label:'Account',ic:'profile',pages:[{id:'profile',label:'Profile'},{id:'devices',label:'Devices'}]},
 {id:'prefs',label:'Preferences',ic:'overlay',pages:[{id:'overlay',label:'In-session overlay'},{id:'streaming',label:'Stream quality'}]},
 {id:'usage',label:'Usage',ic:'storage',pages:[{id:'storage',label:'Storage'},{id:'sessions',label:'My sessions'}]}];
const acSectionOf=page=>ACCOUNT_SECTIONS.find(s=>s.pages.some(x=>x.id===page))||ACCOUNT_SECTIONS[0];
const MY_STORAGE=[
 {app:'Cyberpunk 2077',id:'app-cp2077',gb:6.2,used:'2 hours ago'},
 {app:'Baldur\u2019s Gate 3',id:'app-bg3',gb:3.9,used:'yesterday'},
 {app:'Blender',id:'app-blender',gb:1.8,used:'4 days ago'},
 {app:'Steam',id:'app-steam',gb:0.5,used:'2 hours ago'}];
const MY_DEVICES=[
 {name:'Studio desktop',key:'d1f9c40a8b72e5d3',current:true,trusted:true,caps:['4K H.264','AV1 decode','HEVC decode','VP9','Gamepad API'],measured:'8 Aug 2026',seen:'now',live:true},
 {name:'Living room TV',key:'7ba2e91c05f4d688',current:false,trusted:true,caps:['4K H.264','HEVC decode','Gamepad API'],measured:'2 Aug 2026',seen:'yesterday',live:false},
 {name:'',key:'c30845fe19ab7d21',current:false,trusted:false,caps:['1080p H.264','VP9'],measured:'',seen:'12 days ago',live:false}];
const OVERLAY_ITEMS=[['signal','Connection signal',1],['identity','App name and quality',1],['codec','Codec',0],['metrics','FPS, latency, bitrate',1],['hint','Menu shortcut hint',0],['mic','Microphone',0],['fullscreen','Fullscreen',0]];
const OVERLAY_ACTIONS=[['capture','Capture input',1],['exit','Exit session',1]];

function renderAccountRail(active){
 const item=s=>`<button class="rail-item${s.id===active?' active':''}" onclick="go('#/account/${s.pages[0].id}')" title="${s.label}">${icon(s.ic)}<span class="lbl">${s.label}</span></button>`;
 document.getElementById('rail').innerHTML=
  ACCOUNT_SECTIONS.map(item).join('')
  +`<div class="rail-foot"><button class="rail-item" onclick="toggleRail()" title="Collapse sidebar">${icon('collapse')}<span class="lbl">Collapse</span></button></div>`;
}
const acSec=(t,d,body)=>{AC_D=d;return `<div class="card sec-card">${body}</div>`;};
const acFld=(l,v,h,type)=>`<div class="field"><label class="label">${l}</label><input class="input" ${type?`type="${type}"`:''} value="${v||''}"${type==='password'?' placeholder="\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022"':''}>${h?`<span class="hint">${h}</span>`:''}</div>`;

function acProfile(){
 const fact=(k,v)=>`<div class="ae-fact"><span>${k}</span><span>${v}</span></div>`;
 return acSec('Profile','How your account appears across Quasar.',
  `<div class="rowflex" style="gap:var(--s5);align-items:flex-start">
    <span class="u-ava" style="width:64px;height:64px;font-size:1.6rem;border-radius:var(--r-panel)">SA</span>
    <div style="flex:1;min-width:0">
     <div class="rowflex" style="gap:10px"><h2 style="font-size:var(--t-h2)">${ME.name}</h2><span class="hint">${ME.role} \u00b7 active</span></div>
     <div class="sub mono" style="margin-top:6px">${ME.mail} \u00b7 joined ${ME.joined}</div>
     <div class="ae-facts" style="margin-top:var(--s5);max-width:420px">
      ${fact('Role',ME.role)}${fact('Last sign-in',ME.last)}${fact('Devices',MY_DEVICES.length)}${fact('Managed home',ME.home)}
     </div>
    </div>
   </div>
`)
  +`<div class="card sec-card" style="margin-top:var(--s4)">
    <div class="sec-head"><div><h3>Password</h3><div class="desc">Use at least 12 characters.</div></div></div>
    ${acPwForm()}</div>`;
}

function acOverlay(){
 const sw=(id,l,on)=>`<label class="rowflex" style="justify-content:space-between;padding:7px 0;border-bottom:1px solid var(--line)"><span style="font-size:var(--t-sm)">${l}</span><span class="switch" role="switch" aria-checked="${on?'true':'false'}"></span></label>`;
 return acSec('In-session overlay','The status strip shown over a running session. These settings follow your account to every device you sign in on.',
  `<div class="field"><span class="label">Live preview</span>
    <p class="hint" style="margin:2px 0 10px">What the strip looks like over a running session, using your current settings below. The controls shown are illustrative only.</p>
    <div class="ovprev-stage" aria-hidden="true">
     <div class="session-strip">
      <div class="strip-status">
       <span class="signal" data-q="excellent"><i></i><i></i><i></i><i></i></span>
       <span class="strip-q">Excellent</span>
       <span class="strip-sep"></span>
       <span class="strip-ident"><span class="strip-name">Nebula Raceway</span><span class="strip-tier">1920\u00d71080@60</span></span>
       <span class="strip-codec">H.264</span>
       <span class="strip-sep"></span>
       <span class="strip-metrics">
        <span class="strip-m"><b class="good">60</b><i>fps</i></span>
        <span class="strip-m"><b>18<u>ms</u></b><i>latency</i></span>
        <span class="strip-m"><b>8.4<u>Mb/s</u></b><i>bitrate</i></span>
       </span>
       <span class="strip-hint"><kbd>\u2318</kbd><kbd>M</kbd> menu</span>
      </div>
      <div class="strip-acts">
       <button class="strip-act" tabindex="-1" title="Capture input">${icon('overlay')}</button>
       <button class="strip-act" tabindex="-1" title="Microphone">${icon('streaming')}</button>
       <button class="strip-act" tabindex="-1" title="Fullscreen">${icon('image')}</button>
       <button class="strip-act danger" tabindex="-1" title="Exit session">${icon('back2')}</button>
      </div>
     </div>
    </div>
   </div>
   <div class="field" style="margin-top:var(--s5)"><span class="label">Content</span>
    <div class="segmented" style="align-self:flex-start"><button aria-selected="true">Full</button><button aria-selected="false">Minimal</button><button aria-selected="false">Metrics</button><button aria-selected="false" disabled style="opacity:.5">Custom</button></div></div>
   <div class="grid g2" style="margin-top:var(--s5)">
    <div><div class="eyebrow" style="margin-bottom:6px">Readouts</div>${OVERLAY_ITEMS.map(i=>sw(i[0],i[1],i[2])).join('')}</div>
    <div><div class="eyebrow" style="margin-bottom:6px">Controls</div>${OVERLAY_ACTIONS.map(i=>sw(i[0],i[1],i[2])).join('')}
     <div class="field" style="margin-top:var(--s5)"><span class="label">Position</span><div class="segmented" style="align-self:flex-start"><button aria-selected="true">Top</button><button aria-selected="false">Bottom</button></div></div>
     <div class="field" style="margin-top:var(--s4)"><label class="label">Auto-hide</label><select class="select"><option>After 4 seconds</option><option>After 10 seconds</option><option>Never</option></select></div>
    </div>
   </div>`);
}

function acStreaming(){
 return acSec('Stream quality','Used when launching from the library.',
  `<div class="grid g2" style="max-width:560px">
    <div class="field"><label class="label">Default profile</label>
     <select class="select"><option>Use recommendation</option>${AC.LAUNCH.map(p=>`<option${p.name==='Adaptive 1440p'?' selected':''}>${esc(p.name)}</option>`).join('')}</select>
     <span class="hint">Admin default: Adaptive 1440p</span></div>
   </div>
   <div class="note" style="margin-top:var(--s5)">Your device is measured at sign-in, and a launch never exceeds what it can decode. A profile you pick here is the ceiling, not a guarantee \u2014 see <a onclick="go('#/account/devices')">Devices</a>.</div>
   <div class="rowflex" style="margin-top:var(--s5)"><button class="btn btn-primary">Save stream default</button></div>`);
}

function acStorage(){
 const total=MY_STORAGE.reduce((a,i)=>a+i.gb,0),max=Math.max(...MY_STORAGE.map(i=>i.gb));
 const stat=(l,v)=>`<div><div class="eyebrow">${l}</div><div class="num" style="font-size:var(--t-lg);color:var(--text);margin-top:5px">${v}</div></div>`;
 return acSec('Storage','Managed save-data for your apps, on whichever host ran the session.',
  `<div class="grid g3" style="margin-bottom:var(--s5)">${stat('Apps with storage',MY_STORAGE.length)}${stat('Total used',total.toFixed(1)+' GB')}${stat('Largest app',max.toFixed(1)+' GB')}</div>
   ${tableCard([{l:'App'},{l:'Storage used'},{l:'Last used'},{l:''}],
    MY_STORAGE.map(i=>`<tr>
     <td><a onclick="go('#/library/apps/${i.id}')">${esc(i.app)}</a></td>
     <td style="width:220px"><div class="bar-row"><span class="num" style="min-width:52px;color:var(--text-2)">${i.gb.toFixed(1)} GB</span>${bar(i.gb,max,'')}</div></td>
     <td class="sub mono">${i.used}</td>
     <td class="cell-actions"><button class="btn btn-sm btn-ghost">Clear</button></td></tr>`).join(''))}
   <div class="note" style="margin-top:var(--s4)">Clearing an app's home deletes your saves and settings for it on every host. It cannot be undone, and the home is re-provisioned empty on your next launch.</div>`);
}

function acPwForm(){
 return `<div style="max-width:560px">${acFld('Current password','','','password')}
    <div class="grid g2" style="margin-top:var(--s4)">${acFld('New password','','','password')}${acFld('Confirm new password','','','password')}</div>
   </div>
   <div class="note warn" style="margin-top:var(--s5)">Changing your password signs you out of <strong>every device</strong>, including this one, and ends any session you have running.</div>
   <div class="rowflex" style="margin-top:var(--s5)"><button class="btn btn-primary">Update password</button></div>`;
}

function acDevices(){
 const card=d=>`<div class="dev">
  <div class="rowflex" style="align-items:flex-start;gap:11px">
   <span class="dev-ico">${icon('devices')}</span>
   <div style="flex:1;min-width:0">
    <div class="rowflex" style="gap:8px"><button class="dev-name" title="Rename this device">${esc(d.name||'Device '+d.key.slice(0,8))}</button>${d.current?'<span class="hint">this device</span>':''}</div>
    <div class="sub mono" style="margin-top:2px">${d.key}</div>
   </div>
   ${d.live?`<span class="rowflex" style="gap:7px;flex:none">${sdot('streaming')}<span class="hint">streaming now</span></span>`:''}
  </div>
  <div class="caps">${d.caps.map((c,i)=>`<span class="cap${i===0?' hl':''}">${esc(c)}</span>`).join('')}</div>
  <div class="rowflex" style="justify-content:space-between;padding:10px 0;border-top:1px solid var(--line);border-bottom:1px solid var(--line)">
   <span style="font-size:var(--t-sm)">Trusted device</span><span class="switch" role="switch" aria-checked="${d.trusted}"></span></div>
  <div class="rowflex dev-foot"><span class="hint">${d.measured?'Measured '+d.measured:'Not yet measured'}</span><span class="hint" style="margin-left:auto">Last seen ${d.seen}</span><button class="btn btn-sm btn-ghost">Revoke</button></div>
 </div>`;
 return acSec('Devices','Everything holding a token for your account, and what each one measured itself able to decode.',
  `<div class="dev-grid">${MY_DEVICES.map(card).join('')}</div>
   <div class="note" style="margin-top:var(--s5)">Capabilities are measured at sign-in, not advertised by the device. Revoking signs that device out immediately and ends any session it is running.</div>`);
}

function acSessions(){
 const mine=AC.SESSIONS.filter(s=>s.user===ME.name);
 return acSec('My sessions','Sessions running under your account right now.',
  mine.length?tableCard([{l:'App'},{l:'Host'},{l:'Duration',a:'right'},{l:''}],mine.map(s=>`<tr class="clickable" onclick="go('#/sessions/${s.id}')"><td class="primary">${esc(s.app)}</td><td>${s.host}</td><td class="right num">${s.dur}</td><td class="cell-actions"><button class="btn btn-sm btn-danger">End</button></td></tr>`).join(''))
  :`<div class="empty"><h3>Nothing running</h3><p>Sessions you start from the library appear here while they are live. Ended sessions are kept in the <a onclick="go('#/audit')">audit log</a>.</p></div>`);
}

// The real OverlayPreview measures the strip and scales it down to fit its
// frame (never up), because the strip's width is content-driven. Same rule.
function acFitStrip(){
 const stage=document.querySelector('.ovprev-stage'),strip=stage&&stage.querySelector('.session-strip');
 if(!strip)return;
 strip.style.transform='translate(-50%,-50%)';
 const natural=strip.offsetWidth;if(!natural)return;
 strip.style.transform='translate(-50%,-50%) scale('+Math.min(1,(stage.clientWidth-32)/natural)+')';
}
window.addEventListener('resize',()=>{if(location.hash.indexOf('#/account/overlay')===0)acFitStrip();});
function pageAccount(sub){
 const P={profile:acProfile,overlay:acOverlay,streaming:acStreaming,storage:acStorage,devices:acDevices,sessions:acSessions,security:acProfile};
 const id=P[sub]?sub:'profile';
 const sec=acSectionOf(id);
 renderAccountRail(sec.id);
 const body=P[id]();
 if(id==='overlay')setTimeout(acFitStrip,0);
 const T=sec.pages.map(pg=>({id:pg.id,label:pg.label,go:'#/account/'+pg.id}));
 return `<div class="page" style="max-width:1000px">
 ${head(esc(sec.label),AC_D,'')}
 ${T.length>1?tabs(T,id):''}
 ${body}</div>`;
}
Object.assign(window,{pageAccount,renderAccountRail,ACCOUNT_SECTIONS});
