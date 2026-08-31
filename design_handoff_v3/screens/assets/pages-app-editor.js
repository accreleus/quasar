// Library › app editor. Tabbed to match the real editor's panels:
// Identity · Artwork · Access · Quality · Runtime (+ Library for a provider app).
const AL=window.QDATA;
const AEX='<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" style="width:13px;height:13px"><path d="M4 4l8 8M12 4l-8 8" stroke-linecap="round"/></svg>';
const AE_DEF={desc:'',ref:'',origin:'manual',parent:'',art:null,access:'everyone',grants:[],policy:'inherit',restrict:false,allowed:[],args:[],env:[],home:true,path:'/home/quasar',mounts:[],hosts:'6 / 6',last:'never',sup:[]};
const APP_EX={
 'app-cp2077':{desc:'Open-world RPG. Proton with DXVK and gamepad-first defaults.',ref:'steam:1091500',
  art:{src:'provider',matched:'Cyberpunk 2077',locked:false,attr:'Cover and hero artwork from SteamGridDB.'},
  access:'everyone',grants:[['mara.k','Granted by an admin'],['tobi','Granted by a library sync']],
  policy:'prefer',restrict:true,allowed:['Adaptive 1440p','Competitive 1080p'],
  args:['--fullscreen','--no-vsync','--gamepad-mode=xinput'],
  env:[['PROTON_VERSION','9.0'],['DXVK_HUD','0'],['QUASAR_TARGET_FPS','120']],
  home:true,path:'/home/quasar',mounts:['/mnt/games/cp2077:/games/cp2077'],hosts:'5 / 6',last:'12 minutes ago'},
 'app-blender':{desc:'Creative suite with CUDA and OpenCL runtimes.',
  art:{src:'manual',matched:'',locked:true,attr:'Uploaded by salty2011 on 4 August 2026.'},
  access:'specific',grants:[['mara.k','Granted by an admin'],['priya','Granted by an admin']],
  policy:'force',args:['--factory-startup'],env:[['CUDA_VISIBLE_DEVICES','0']],
  home:true,path:'/home/quasar',mounts:['/mnt/projects:/projects'],hosts:'2 / 6',last:'4 hours ago'},
 'app-steam':{desc:'The Steam client itself. Users launch it to install titles into their managed home.',
  art:{src:'none',matched:'',locked:false,attr:''},policy:'inherit',
  args:['-silent','-noreactlogin'],env:[['STEAM_STARTUP_FLAGS','']],
  home:true,path:'/home/quasar',mounts:[],hosts:'6 / 6',last:'31 minutes ago',
  sup:[['Steam Linux Runtime','1391110','built-in denylist','4 users','last seen 2 hours ago',true],
       ['Proton Experimental','1493710','built-in denylist','4 users','last seen 2 hours ago',false],
       ['Wallpaper Engine','431960','ignored by salty2011','2 users','last seen yesterday',false],
       ['Steamworks Common Redistributables','228980','built-in denylist','3 users','last seen 2 hours ago',false]]},
 'app-hades2':{desc:'Discovered under the Steam client on quasar-node-2.',ref:'steam:1145350',origin:'discovered',parent:'Steam',
  art:{src:'provider',matched:'Hades II',locked:false,attr:'Cover and hero artwork from SteamGridDB.'},
  policy:'inherit',hosts:'5 / 6',last:'never'}};
const aeX=a=>Object.assign({},AE_DEF,APP_EX[a.id]||{});
const aeGlyph=n=>n.split(/[\s:]+/).filter(Boolean).slice(0,2).map(w=>w[0]).join('').toUpperCase();
const aeFld=(l,v,h,mono,ph)=>`<div class="field"><label class="label">${l}</label><input class="input${mono?' mono':''}" value="${esc(v||'')}"${ph?` placeholder="${esc(ph)}"`:''}>${h?`<span class="hint">${h}</span>`:''}</div>`;
const aeSel=(l,opts,val,h)=>`<div class="field"><label class="label">${l}</label><select class="select">${opts.map(o=>`<option${o===val?' selected':''}>${esc(o)}</option>`).join('')}</select>${h?`<span class="hint">${h}</span>`:''}</div>`;
const aeSec=(t,d,body)=>`<div class="fsec"><div class="fs-label"><h4>${t}</h4><p>${d}</p></div><div class="fs-fields">${body}</div></div>`;
const aeFact=(k,v)=>`<div class="ae-fact"><span>${k}</span><span>${v}</span></div>`;
const aeSeg=(a,b,first)=>`<div class="segmented" style="align-self:flex-start"><button aria-selected="${!!first}">${a}</button><button aria-selected="${!first}">${b}</button></div>`;
const aeRows=(items,ph,two,add)=>`<div class="kv">${items.map(v=>two
 ?`<div class="kv-row"><input class="input mono" value="${esc(v[0])}" style="max-width:200px"><input class="input mono" value="${esc(v[1])}"><button class="kv-del">${AEX}</button></div>`
 :`<div class="kv-row"><input class="input mono" value="${esc(v)}" placeholder="${esc(ph||'')}"><button class="kv-del">${AEX}</button></div>`).join('')}<button class="btn btn-ghost btn-sm">${icon('plus')}${add}</button></div>`;

function aeIdentity(a,x){
 return aeSec('Identity','How the app appears in the library.',
  aeFld('Display name',a.name)
  +aeFld('Description',x.desc)
  +`<div class="grid g2">`
   +aeSel('Kind',['Game','Desktop','Launcher','Tool'],a.kind,'Presentation only — never affects scheduling, streaming or admission.')
   +aeSel('Library provider',['None','Steam'],a.provider==='steam'?'Steam':'None','Marks this app as a library-discovery source. Steam is the only provider today.')
  +`</div>`
  +(a.provider==='steam'?`<div class="note">Setting a provider is what triggers discovery — not Kind. Titles found installed under this app's managed home are published as their own tiles.</div>`:''))
 +aeSec('Provenance','Where this tile came from. Read-only — this is identity, not configuration.',
  `<div class="ae-facts" style="max-width:560px">
   ${aeFact('External reference',x.ref?`<span class="mono">${esc(x.ref)}</span>`:'<span style="color:var(--text-4)">not a provider title</span>')}
   ${aeFact('Origin',x.origin==='discovered'?'Discovered by a library sync':'Created by hand')}
   ${aeFact('Parent tile',x.parent?`<a onclick="go('#/library/apps/app-steam')">${esc(x.parent)}</a>`:'<span style="color:var(--text-4)">—</span>')}
   ${aeFact('Slug',`<span class="mono">${esc(a.id)}</span>`)}
  </div>`
  +(x.parent?`<div class="note">A discovered tile carries no runtime of its own. Image, arguments, environment and mounts are merged from <strong>${esc(x.parent)}</strong> at launch, so an edit to the parent reaches every tile under it with no re-sync.</div>`
   :`<div class="hint">An external reference lets artwork resolve by id instead of by fuzzy title. Nothing else reads it.</div>`));
}

function aeArtwork(a,x){
 const g=aeGlyph(a.name),art=x.art;
 const SRC={provider:'Matched automatically',manual:'Set by an admin',none:'No artwork found — a games database has no entry for it'};
 const artless=a.kind==='Desktop'||a.kind==='Launcher'||a.kind==='Tool';
 const cands=[a.name,a.name+': Phantom Liberty',a.name+' REDmod','Edgerunners'];
 return aeSec('Artwork','The library tile and the wider hero banner. Two separate crops — a tile stretched into the hero frame reads as a blown-up thumbnail.',
  `<div class="ae-crops">
   <figure class="ae-crop"><div class="ae-frame tile">${g}</div><figcaption>Tile · 2:3</figcaption></figure>
   <figure class="ae-crop"><div class="ae-frame hero">${g}</div><figcaption>Hero · wide</figcaption></figure>
  </div>
  <div class="ae-facts" style="max-width:560px">
   ${aeFact('Source',art?esc(SRC[art.src]):'No artwork — showing the gradient tile')}
   ${art&&art.matched?aeFact('Matched',`“${esc(art.matched)}”`):''}
   ${aeFact('Automatic matching',art&&art.locked?'Locked — an admin override, never overwritten':'Runs on the next sweep')}
   ${art&&art.attr?aeFact('Credit',esc(art.attr)):''}
  </div>
  ${artless?`<div class="note">This is a <strong>${esc(a.kind)}</strong> app, so it is never looked up automatically — a games database will not have an entry for it. Upload artwork or paste an image URL below.</div>`:''}`)
 +aeSec('Set artwork','Fuzzy matching is wrong sometimes, and a desktop app is not in a games database at all. Both overrides live here.',
  `<div class="field"><label class="label">Search the artwork provider</label>
   <div style="display:flex;gap:8px;max-width:560px"><input class="input" value="${esc(a.name)}"><button class="btn">${icon('search')}Search</button><button class="btn btn-ghost">Match automatically</button></div>
   <span class="hint">Matching is fuzzy. Check the title before accepting a result.</span></div>
  <ul class="ae-cands">${cands.map(c=>`<li><button class="ae-cand"><i>${aeGlyph(c)}</i><b>${esc(c)}</b></button></li>`).join('')}</ul>
  <div class="rowflex" style="flex-wrap:wrap"><button class="btn">Upload tile image</button><button class="btn">Upload hero image</button>${art?`<button class="btn btn-ghost">Reset to gradient</button>`:''}</div>
  <div class="grid g2" style="max-width:560px">${aeFld('Tile image URL','','',0,'https://…')}${aeFld('Hero image URL','','',0,'https://…')}</div>
  <div class="rowflex"><button class="btn">Fetch from URL</button><span class="hint">Fetched once and cached here — an image is never hotlinked. Public http/https addresses only.</span></div>`);
}

function aeAccess(a,x){
 const everyone=x.access==='everyone';
 const names=AL.USERS.map(u=>u.name).filter(n=>!x.grants.some(g=>g[0]===n));
 return aeSec('Access','Who may see and launch this app. Enforced by the server on every request — this control is convenience, not the boundary.',
  aeSeg('Everyone','Specific users',everyone)
  +`<div class="field" style="max-width:560px"><span class="label">Specific grants</span>
   ${everyone?`<span class="hint">Everyone can already see this app. These grants have no additional effect unless Everyone access is later removed.</span>`:''}
   <div class="ae-list">${x.grants.length?x.grants.map(g=>`<div class="ae-item"><div><div class="ae-item-t">${esc(g[0])}</div><div class="ae-item-m">${esc(g[1])}</div></div><button class="btn btn-ghost btn-sm">Revoke</button></div>`).join(''):`<div class="hint">No individual grants.</div>`}</div></div>
  <div class="rowflex" style="align-items:flex-end;max-width:560px"><div style="flex:1">${aeSel('Add a user',['Choose a user'].concat(names),'Choose a user')}</div><button class="btn">Grant access</button></div>
  ${x.grants.some(g=>/sync/.test(g[1]))?`<div class="note warn">One grant here came from a library sync. Revoking it holds only until the next sync — ignore the appid under <a onclick="go('#/library/apps/app-steam/library')">Steam › Library</a> to stop it coming back.</div>`:''}`);
}

function aeQuality(a,x){
 const POL={inherit:'Use global or account default',prefer:'Use an app default launch profile',force:'Force an app launch profile'};
 const p=AL.LAUNCH.find(q=>q.name===a.launch)||AL.LAUNCH[0];
 const r0=p.rungs[0].split(' · '),h0=r0[0].split(' ');
 const pill=`<span class="spec-pill"><span>${esc(h0[0])}</span><span>${esc(h0.slice(1).join(' '))}</span><span>${esc(r0[1]||'')}</span></span>`;
 const rungs=`<div class="field"><span class="label">Rungs, in order</span><div>${p.rungs.map((r,i)=>`<div class="rowflex" style="padding:7px 0;border-bottom:1px solid var(--line)"><span class="num" style="color:var(--text-4);font-size:var(--t-xs)">${i+1}</span><span style="font-size:var(--t-sm)">${esc(r)}</span></div>`).join('')}</div><span class="hint">Advertised, not resolved — a launch may fall through to a lower rung.</span></div>`;
 return aeSec('Quality profile','How this app chooses stream quality.',
  aeSel('Source',[POL.inherit,POL.prefer,POL.force],POL[x.policy],'Launch profiles are defined once under Streaming and reused globally or per app.')
  +(x.policy==='inherit'?`<div class="note">This app follows the account default, then the global default. Nothing is pinned here.</div>`
   :aeSel('App launch profile',AL.LAUNCH.map(q=>q.name),a.launch,x.policy==='force'?'Users cannot choose a different launch profile for this app.':'Used before account and global defaults.')+pill+rungs))
 +(x.policy==='force'
  ?aeSec('Launch options','Which launch profiles users can choose from the menu beside Play.',
   `<div class="note">This app forces <strong>${esc(a.launch)}</strong>, so there is nothing for a user to choose and no allow-list can apply.</div>`)
  :aeSec('Launch options','Which launch profiles users can choose from the menu beside Play. Always intersected with what their device can actually handle.',
   aeSeg('Any eligible profile','Only these',!x.restrict)
   +(x.restrict?`<div class="field">${AL.LAUNCH.map(q=>{const pin=q.name===a.launch&&x.policy!=='inherit';return `<label class="ae-check" style="padding:5px 0"><input type="checkbox"${pin||x.allowed.includes(q.name)?' checked':''}${pin?' disabled':''}>${esc(q.name)}${pin?'<span class="chip chip-accent">default</span>':''}</label>`}).join('')}<span class="hint">The app default is always launchable and cannot be unticked.</span></div>`:'')
   +`<div class="hint">A menu filter, never the enforcement — the server rejects a disallowed profile on every launch regardless of what a client sends.</div>`));
}

function aeRuntime(a,x){
 if(x.parent)return aeSec('Runtime','Merged from the parent tile at launch.',
  `<div class="note">This tile is discovered under <strong>${esc(x.parent)}</strong> and contributes no runtime of its own — image, arguments, environment and mounts all come from the parent. Edit <a onclick="go('#/library/apps/app-steam/runtime')">${esc(x.parent)}</a> to change what it launches with.</div>
   <div class="ae-facts" style="max-width:560px">${aeFact('Image',`<span class="mono">${esc(a.img)}</span>`)}${aeFact('Runtime preset',`<a onclick="go('#/library/presets')">${esc(a.preset)}</a>`)}${aeFact('Own runtime spec','<span class="mono">{}</span>')}</div>`);
 return aeSec('Runtime','Image, arguments and environment. What you set here layers on top of the preset.',
  aeSel('Runtime preset',['None — configure everything here'].concat(AL.PRESETS.map(p=>p.name)),a.preset,'Reusable image, environment and storage defaults, managed under Runtime presets.')
  +`<div class="rowflex"><button class="btn btn-ghost btn-sm">Extract to preset</button><span class="hint">Moves this app's inline runtime into a new shared preset.</span></div>
   <div class="note">Values below <strong>layer on top of the preset</strong>. Environment merges with the app winning on a shared key; mounts and launch arguments append.</div>`
  +aeFld('Image',a.img,'Leave blank to use the preset image.',1,'ghcr.io/…')
  +`<div class="field"><span class="label">Launch arguments</span>${aeRows(x.args,'--flag=value',0,'Add argument')}</div>`
  +`<div class="field"><span class="label">Environment variables</span><span class="hint">Merged over the preset's. A key set here wins — 3 of the preset's 7 keys overridden.</span>${aeRows(x.env,'',1,'Add variable')}</div>`)
 +aeSec('Storage','A persistent home directory per user, plus anything extra this app needs mounted.',
  `<label class="rowflex" style="gap:10px;font-size:var(--t-sm);font-weight:600"><span class="switch" role="switch" aria-checked="${x.home}"></span>Managed home</label>
   <span class="hint">Provisions a per-user home mounted into the container. Data lives outside the container, under the storage root of whichever host runs the session.</span>
   ${x.home?aeFld('Mount path inside container',x.path,'Where the home appears inside the container — not where the data is stored.',1,'/home/quasar'):''}
   <div class="ae-facts" style="max-width:560px">${aeFact('Storage backend','host directories under each host’s storage root')}${aeFact('Set per host','<a onclick="go(\'#/fleet/hosts\')">Fleet › Hosts</a>')}</div>
   <div class="field"><span class="label">Extra mounts</span>${aeRows(x.mounts,'/host/path:/container/path',0,'Add mount')}<span class="hint">Appended to whatever the runtime preset already mounts.</span></div>`);
}

function aeLibrary(a,x){
 return aeSec('Library','Steam appids this instance has seen installed for at least one user that have no live library tile. A read and a button, not a review queue — discovery works correctly whether or not it is ever opened.',
  `<div class="ae-list" style="max-width:640px">${x.sup.length?x.sup.map(s=>`<div class="ae-item"><div><div class="ae-item-t">${esc(s[0])}</div><div class="ae-item-m mono">${esc(s[1])} · ${esc(s[2])} · ${esc(s[3])} · ${esc(s[4])}${s[5]?' · a disabled tile already exists':''}</div></div><button class="btn btn-ghost btn-sm">Un-ignore</button></div>`).join(''):`<div class="hint">Nothing suppressed right now — every appid this instance has observed is either published or has never been scanned.</div>`}</div>
   <div class="note">Suppression is a hide, never a delete: an ignored appid keeps its tile, its artwork and every user's favourite of it. Un-ignoring republishes it to every user who has it installed.</div>`);
}

function aeRail(a,x){
 return `<div class="ae-rail">
 <div class="card" style="overflow:hidden">
  <div class="ae-frame hero" style="border:0;border-radius:0">${aeGlyph(a.name)}</div>
  <div class="card-pad" style="display:flex;flex-direction:column;gap:var(--s4)">
   <div class="rowflex" style="justify-content:space-between"><span class="label">Enabled for users</span><span class="switch" role="switch" aria-checked="${a.enabled}"></span></div>
   <div class="ae-facts">
    ${aeFact('Sessions · 30d',`<span class="num">${a.sessions30}</span>`)}
    ${aeFact('Image present on',`<a onclick="go('#/library/images')" class="num">${esc(x.hosts)} hosts</a>`)}
    ${aeFact('Last launched',esc(x.last))}
    ${aeFact('Runtime preset',`<a onclick="go('#/library/presets')">${esc(a.preset)}</a>`)}
    ${aeFact('Launch profile',esc(a.launch))}
    ${aeFact('Source',esc(a.src))}
   </div>
  </div>
 </div>
 <button class="btn btn-danger" style="width:100%;justify-content:center">Delete app</button>
 <p class="hint">Deleting cascades: every user's favourite of this tile and its artwork go with it. Disable it above to hide it from the library instead.</p>
</div>`;
}

function pageAppEditor(path){
 const parts=String(path).split('/');
 const a=AL.APPS.find(v=>v.id===parts[0])||AL.APPS[0];
 const x=aeX(a);
 const base='#/library/apps/'+a.id;
 const T=[{id:'identity',label:'Identity',go:base},
  {id:'artwork',label:'Artwork',go:base+'/artwork'},
  {id:'access',label:'Access',count:x.grants.length||null,go:base+'/access'},
  {id:'quality',label:'Quality',go:base+'/quality'},
  {id:'runtime',label:'Runtime',go:base+'/runtime'}];
 if(a.provider==='steam')T.push({id:'library',label:'Library',count:x.sup.length,go:base+'/library'});
 const active=T.some(t=>t.id===parts[1])?parts[1]:'identity';
 const body={identity:aeIdentity,artwork:aeArtwork,access:aeAccess,quality:aeQuality,runtime:aeRuntime,library:aeLibrary}[active](a,x);
 return `<div class="page">
 ${head(esc(a.name),`${esc(a.kind)} · ${esc(a.src)} · ${a.sessions30} sessions in the last 30 days`,
  `<button class="btn btn-ghost">Duplicate</button><button class="btn btn-ghost">Discard</button><button class="btn btn-primary">Save changes</button>`,
  `<a onclick="go('#/library/apps')">Library</a>${icon('chev')}<span>${esc(a.name)}</span>`)}
 <div class="editor">
  <div class="card ae-panel">${tabs(T,active)}${body}</div>
  ${aeRail(a,x)}
 </div></div>`;
}
Object.assign(window,{pageAppEditor});
