// Demo dataset — a busy self-hosted fleet.
const HOSTS=[
 {id:'c2059601',name:'quasar-node-1',state:'online',cpu:'AMD Ryzen 9 9950X3D 16-Core',ram:'128 GB',ramPct:41,gpus:[{n:'GeForce RTX 5090',v:'NVIDIA',vram:[21.4,32],slots:[2,3]}],storeFree:96,storeTotal:120,sessions:2,hb:'4s ago',agent:'0.1.0',uptime:'18d 4h'},
 {id:'8fa41c02',name:'quasar-node-2',state:'online',cpu:'AMD Ryzen 9 7950X 16-Core',ram:'128 GB',ramPct:63,gpus:[{n:'GeForce RTX 4090',v:'NVIDIA',vram:[19.8,24],slots:[2,3]},{n:'GeForce RTX 4090',v:'NVIDIA',vram:[6.1,24],slots:[1,3]}],storeFree:412,storeTotal:960,sessions:3,hb:'2s ago',agent:'0.1.0',uptime:'18d 4h'},
 {id:'71bd9e33',name:'quasar-node-3',state:'online',cpu:'Intel Core i9-14900K',ram:'96 GB',ramPct:28,gpus:[{n:'Radeon RX 7900 XTX',v:'AMD',vram:[8.2,24],slots:[1,2]}],storeFree:64,storeTotal:480,sessions:1,hb:'3s ago',agent:'0.1.0',uptime:'6d 11h'},
 {id:'a4c7f210',name:'quasar-node-4',state:'draining',cpu:'AMD Ryzen 7 7800X3D',ram:'64 GB',ramPct:52,gpus:[{n:'GeForce RTX 4080 Super',v:'NVIDIA',vram:[11.9,16],slots:[1,2]}],storeFree:208,storeTotal:480,sessions:1,hb:'5s ago',agent:'0.1.0',uptime:'2d 9h'},
 {id:'d0e13b77',name:'quasar-node-5',state:'degraded',cpu:'Intel Core i7-13700K',ram:'64 GB',ramPct:19,gpus:[{n:'GeForce RTX 4070 Ti',v:'NVIDIA',vram:[0,12],slots:[0,2]}],storeFree:12,storeTotal:240,sessions:0,hb:'41s ago',agent:'0.0.9',uptime:'11h 3m'},
 {id:'2b6690af',name:'quasar-node-6',state:'offline',cpu:'AMD Ryzen 5 7600X',ram:'32 GB',ramPct:0,gpus:[{n:'GeForce RTX 4060 Ti',v:'NVIDIA',vram:[0,16],slots:[0,2]}],storeFree:180,storeTotal:240,sessions:0,hb:'14m ago',agent:'0.0.9',uptime:'—'}];
const SESSIONS=[
 {id:'s-9f31a2c4',user:'mara.k',app:'Cyberpunk 2077',host:'quasar-node-2',gpu:'RTX 4090 #0',state:'streaming',fps:118,fpsT:[112,118,120,119,117,120,118,116,119,118],lat:14,br:38.2,codec:'AV1',res:'2560×1440',dur:'1h 12m',q:'excellent'},
 {id:'s-77b0e1d9',user:'devon',app:'Blender',host:'quasar-node-1',gpu:'RTX 5090 #0',state:'streaming',fps:60,fpsT:[60,60,59,60,60,60,58,60,60,60],lat:9,br:22.0,codec:'AV1',res:'3840×2160',dur:'3h 41m',q:'excellent'},
 {id:'s-1c48f60b',user:'ana.rs',app:'Baldur\u2019s Gate 3',host:'quasar-node-2',gpu:'RTX 4090 #1',state:'streaming',fps:94,fpsT:[98,96,92,88,94,95,93,90,94,94],lat:26,br:31.4,codec:'HEVC',res:'2560×1440',dur:'47m',q:'good'},
 {id:'s-4e2ac015',user:'tobi',app:'Helldivers 2',host:'quasar-node-3',gpu:'RX 7900 XTX',state:'degraded',fps:52,fpsT:[72,68,61,55,49,52,47,53,50,52],lat:68,br:14.8,codec:'H.264',res:'1920×1080',dur:'22m',q:'poor'},
 {id:'s-63d7cc81',user:'mara.k',app:'Windows Desktop',host:'quasar-node-1',gpu:'RTX 5090 #0',state:'streaming',fps:60,fpsT:[60,60,60,60,59,60,60,60,60,60],lat:11,br:12.6,codec:'AV1',res:'1920×1080',dur:'5h 02m',q:'excellent'},
 {id:'s-05fe9b3a',user:'jules',app:'Elden Ring',host:'quasar-node-2',gpu:'RTX 4090 #0',state:'streaming',fps:60,fpsT:[60,60,60,58,60,60,60,60,59,60],lat:19,br:26.9,codec:'HEVC',res:'2560×1440',dur:'1h 55m',q:'good'},
 {id:'s-9a0c4412',user:'sam.w',app:'Factorio',host:'quasar-node-4',gpu:'RTX 4080 S',state:'connecting',fps:0,fpsT:[0,0,0,0,0,0,0,0,0,0],lat:0,br:0,codec:'—',res:'1920×1080',dur:'8s',q:'fair'},
 {id:'s-38ee71d0',user:'priya',app:'DaVinci Resolve',host:'quasar-node-1',gpu:'RTX 5090 #0',state:'ended',fps:0,fpsT:[60,60,60,59,60,60,58,60,0,0],lat:0,br:0,codec:'AV1',res:'3840×2160',dur:'2h 08m',q:'good'},
 {id:'s-b1197fe6',user:'tobi',app:'Cyberpunk 2077',host:'quasar-node-3',gpu:'RX 7900 XTX',state:'failed',fps:0,fpsT:[88,84,70,42,0,0,0,0,0,0],lat:0,br:0,codec:'HEVC',res:'2560×1440',dur:'4m',q:'poor'}];
const APPS=[
 {id:'app-cp2077',name:'Cyberpunk 2077',kind:'Game',src:'Steam',preset:'Proton GPU',launch:'Adaptive 1440p',enabled:true,sessions30:214,img:'ghcr.io/quasar/proton:9.0'},
 {id:'app-bg3',name:'Baldur\u2019s Gate 3',kind:'Game',src:'Steam',preset:'Proton GPU',launch:'Adaptive 1440p',enabled:true,sessions30:187,img:'ghcr.io/quasar/proton:9.0'},
 {id:'app-elden',name:'Elden Ring',kind:'Game',src:'Steam',preset:'Proton GPU',launch:'Adaptive 1440p',enabled:true,sessions30:143,img:'ghcr.io/quasar/proton:9.0'},
 {id:'app-hd2',name:'Helldivers 2',kind:'Game',src:'Steam',preset:'Proton GPU',launch:'Competitive 1080p',enabled:true,sessions30:96,img:'ghcr.io/quasar/proton:9.0'},
 {id:'app-factorio',name:'Factorio',kind:'Game',src:'Steam',preset:'Native Linux',launch:'Adaptive 1440p',enabled:true,sessions30:61,img:'ghcr.io/quasar/native:3'},
 {id:'app-blender',name:'Blender',kind:'Desktop',src:'Manual',preset:'Workstation 4K',launch:'Workstation 4K',enabled:true,sessions30:52,img:'ghcr.io/quasar/blender:4.2'},
 {id:'app-resolve',name:'DaVinci Resolve',kind:'Desktop',src:'Manual',preset:'Workstation 4K',launch:'Workstation 4K',enabled:true,sessions30:33,img:'ghcr.io/quasar/resolve:19'},
 {id:'app-windows',name:'Windows Desktop',kind:'Desktop',src:'Manual',preset:'Windows VM',launch:'Adaptive 1080p',enabled:true,sessions30:28,img:'ghcr.io/quasar/winvm:11'},
 {id:'app-hades2',name:'Hades II',kind:'Game',src:'Steam',preset:'Proton GPU',launch:'Adaptive 1440p',enabled:false,sessions30:0,img:'ghcr.io/quasar/proton:9.0'},
 {id:'app-testcard',name:'Diagnostics test card',kind:'Tool',src:'Manual',preset:'Native Linux',launch:'Competitive 1080p',enabled:true,sessions30:19,img:'ghcr.io/quasar/diag:0.4'},
 {id:'app-steam',name:'Steam',kind:'Launcher',src:'Manual',preset:'Proton GPU',launch:'Adaptive 1440p',enabled:true,sessions30:74,img:'ghcr.io/quasar/steam:1.4',provider:'steam'}];
const USERS=[
 {id:'u-01',name:'salty2011',mail:'admin@quasar.local',role:'Admin',state:'active',last:'now',sessions:41,home:'12.4 GB'},
 {id:'u-02',name:'mara.k',mail:'mara@studio.io',role:'User',state:'active',last:'streaming',sessions:96,home:'214 GB'},
 {id:'u-03',name:'devon',mail:'devon@studio.io',role:'User',state:'active',last:'streaming',sessions:64,home:'88 GB'},
 {id:'u-04',name:'ana.rs',mail:'ana@studio.io',role:'User',state:'active',last:'streaming',sessions:37,home:'41 GB'},
 {id:'u-05',name:'tobi',mail:'tobi@studio.io',role:'User',state:'active',last:'2m ago',sessions:52,home:'156 GB'},
 {id:'u-06',name:'jules',mail:'jules@studio.io',role:'User',state:'active',last:'streaming',sessions:23,home:'62 GB'},
 {id:'u-07',name:'priya',mail:'priya@studio.io',role:'Admin',state:'active',last:'14m ago',sessions:18,home:'304 GB'},
 {id:'u-08',name:'sam.w',mail:'sam@studio.io',role:'User',state:'active',last:'connecting',sessions:9,home:'27 GB'},
 {id:'u-09',name:'kenji',mail:'kenji@studio.io',role:'User',state:'disabled',last:'21d ago',sessions:4,home:'11 GB'}];
const INVITES=[
 {code:'QSR-7K2P-9XM4',by:'salty2011',created:'2h ago',expires:'in 5d',state:'pending',uses:'0 / 1'},
 {code:'QSR-4A1D-BB07',by:'priya',created:'yesterday',expires:'in 4d',state:'pending',uses:'0 / 1'},
 {code:'QSR-9Z3Q-KK18',by:'salty2011',created:'6d ago',expires:'expired',state:'expired',uses:'0 / 1'},
 {code:'QSR-2M8N-TT55',by:'salty2011',created:'12d ago',expires:'—',state:'redeemed',uses:'1 / 1'},
 {code:'QSR-5C0V-RR92',by:'priya',created:'19d ago',expires:'—',state:'revoked',uses:'0 / 1'}];
const AUDIT=[
 {t:'14:02:11',actor:'salty2011',action:'host.drain',target:'quasar-node-4',detail:'reason=maintenance window 14:00–16:00',sev:'warn'},
 {t:'13:58:40',actor:'system',action:'session.failed',target:'s-b1197fe6',detail:'encoder_init_failed: vaapi device busy',sev:'err'},
 {t:'13:44:02',actor:'priya',action:'app.updated',target:'Cyberpunk 2077',detail:'launch_profile: Adaptive 1080p → Adaptive 1440p',sev:'info'},
 {t:'13:21:57',actor:'salty2011',action:'invite.created',target:'QSR-7K2P-9XM4',detail:'expires_in=7d max_uses=1',sev:'info'},
 {t:'12:57:13',actor:'system',action:'host.degraded',target:'quasar-node-5',detail:'heartbeat late 41s; gpu enumeration returned 0 devices',sev:'err'},
 {t:'12:30:08',actor:'devon',action:'session.started',target:'s-77b0e1d9',detail:'app=Blender host=quasar-node-1 tier=workstation-4k',sev:'info'},
 {t:'11:48:22',actor:'priya',action:'user.role_changed',target:'kenji',detail:'role: user → disabled',sev:'warn'},
 {t:'10:12:44',actor:'salty2011',action:'settings.updated',target:'instance',detail:'registration_mode: closed → invite_only',sev:'info'}];
const STORAGE=[
 {user:'mara.k',host:'quasar-node-2',provider:'Docker volume',size:214,quota:512,state:'active'},
 {user:'priya',host:'quasar-node-1',provider:'Local directory',size:304,quota:512,state:'active'},
 {user:'tobi',host:'quasar-node-3',provider:'Docker volume',size:156,quota:256,state:'active'},
 {user:'devon',host:'quasar-node-1',provider:'Docker volume',size:88,quota:256,state:'active'},
 {user:'jules',host:'quasar-node-2',provider:'Docker volume',size:62,quota:256,state:'active'},
 {user:'ana.rs',host:'quasar-node-2',provider:'Docker volume',size:41,quota:256,state:'active'},
 {user:'sam.w',host:'quasar-node-4',provider:'Local directory',size:27,quota:128,state:'active'},
 {user:'kenji',host:'quasar-node-3',provider:'Docker volume',size:11,quota:128,state:'pending cleanup'}];
const IMAGES=[
 {name:'Steam',desc:'Steam in a game-console experience through nested Gamescope (legacy Big Picture, the games-on-whales-validated game-foreground path). Needs a host --shm-size bump, 32-bit graphics libraries, and PUID/PGID matching the managed home.',kind:'Prebuilt',version:'2026.08.07',ref:'ghcr.io/quasar/steam:2026.08.07',digest:'sha256:8f2a91c4e7b3',pulled:'8 Aug 2026, 09:22',presets:['Proton GPU'],apps:['Steam'],installed:'2026.08.07',state:'installed',preset:true,provider:'Steam',pinned:false,hosts:[['quasar-node-1','ready'],['quasar-node-2','ready'],['quasar-node-3','ready'],['quasar-node-4','ready'],['quasar-node-5','ready']]},
 {name:'Proton runtime',desc:'Wine/Proton layer for Windows titles, with DXVK and VKD3D preinstalled. Pairs with the Proton GPU runtime preset.',kind:'Prebuilt',version:'9.0.4',ref:'ghcr.io/quasar/proton:9.0',digest:'sha256:41c9de07ab55',pulled:'2 Aug 2026, 14:05',presets:['Proton GPU'],apps:['Cyberpunk 2077','Baldur\u2019s Gate 3','Elden Ring','Helldivers 2','Hades II'],installed:'9.0.2',state:'update',preset:true,provider:'Manual',pinned:false,hosts:[['quasar-node-1','ready'],['quasar-node-2','ready'],['quasar-node-3','ready'],['quasar-node-4','ready'],['quasar-node-5','stale']]},
 {name:'Native Linux',desc:'Minimal Vulkan + PipeWire base for titles that ship a native Linux build. No translation layer.',kind:'Prebuilt',version:'3.1.0',ref:'ghcr.io/quasar/native:3',digest:'sha256:0b77e5a1c930',pulled:'21 Jul 2026, 11:47',presets:['Native Linux'],apps:['Factorio'],installed:'3.1.0',state:'installed',preset:true,provider:'Manual',pinned:true,hosts:[['quasar-node-1','ready'],['quasar-node-2','ready'],['quasar-node-3','ready'],['quasar-node-4','ready'],['quasar-node-5','ready']]},
 {name:'Workstation',desc:'Creative-tool base with the NVIDIA userspace driver stack, CUDA and OpenCL runtimes. Used by Blender and DaVinci Resolve.',kind:'Prebuilt',version:'4.2.1',ref:'ghcr.io/quasar/blender:4.2',digest:'sha256:c6a4f1092e8d',pulled:'29 Jul 2026, 16:12',presets:['Workstation 4K'],apps:['Blender','DaVinci Resolve'],installed:'4.2.1',state:'installed',preset:true,provider:'Manual',pinned:false,hosts:[['quasar-node-1','ready'],['quasar-node-2','ready']]},
 {name:'Windows VM',desc:'QEMU/KVM guest with GPU passthrough for titles with anti-cheat that refuses Proton. Requires IOMMU on the host.',kind:'Prebuilt',version:'11.0.0',ref:'ghcr.io/quasar/winvm:11',digest:'sha256:9e3b70fd4a16',pulled:'\u2014',presets:['Windows VM'],apps:['Windows Desktop'],installed:'',state:'installing',preset:false,provider:'Manual',pinned:false,hosts:[['quasar-node-2','pulling']]},
 {name:'Diagnostics test card',desc:'Deterministic moving test pattern with an embedded timecode. Used to certify encode and decode paths after a host change.',kind:'Prebuilt',version:'0.4.0',ref:'ghcr.io/quasar/diag:0.4',digest:'sha256:75d0c8e2fb41',pulled:'\u2014',presets:['Native Linux'],apps:['Diagnostics test card'],installed:'',state:'available',preset:false,provider:'Manual',pinned:false,hosts:[]},
 {name:'Proton runtime 8 (legacy)',desc:'Superseded Wine/Proton layer, kept while a few titles were pinned to the 8.x branch. Nothing points at it now.',kind:'Prebuilt',version:'8.0.5',ref:'ghcr.io/quasar/proton:8.0',digest:'sha256:2ad61bf07c94',pulled:'14 Mar 2026, 08:40',presets:[],apps:[],installed:'8.0.5',state:'installed',preset:false,provider:'Manual',pinned:false,hosts:[['quasar-node-1','ready'],['quasar-node-2','ready'],['quasar-node-3','ready'],['quasar-node-4','ready'],['quasar-node-5','ready']]}];
const PRESETS=[
 {name:'Proton GPU',img:'ghcr.io/quasar/proton:9.0',env:7,mounts:2,gpu:true,used:5},
 {name:'Native Linux',img:'ghcr.io/quasar/native:3',env:3,mounts:1,gpu:true,used:2},
 {name:'Workstation 4K',img:'ghcr.io/quasar/blender:4.2',env:5,mounts:3,gpu:true,used:2},
 {name:'Windows VM',img:'ghcr.io/quasar/winvm:11',env:9,mounts:2,gpu:true,used:1}];
const LAUNCH=[
 {name:'Adaptive 1440p',def:true,used:5,rungs:['AV1 1440p60 · 35 Mb/s','HEVC 1440p60 · 30 Mb/s','H.264 1080p60 · 20 Mb/s']},
 {name:'Competitive 1080p',def:false,used:2,rungs:['AV1 1080p120 · 28 Mb/s','HEVC 1080p120 · 25 Mb/s','H.264 1080p60 · 18 Mb/s']},
 {name:'Workstation 4K',def:false,used:2,rungs:['AV1 2160p60 · 45 Mb/s','HEVC 2160p60 · 40 Mb/s','H.264 1440p60 · 24 Mb/s']},
 {name:'Adaptive 1080p',def:false,used:1,rungs:['HEVC 1080p60 · 22 Mb/s','H.264 1080p60 · 16 Mb/s']}];
const STREAMP=[
 {name:'av1-2160p60',codec:'AV1',res:'3840×2160',fps:60,br:45,floor:18,enc:'NVENC · VA-API',browser:'Chrome 116+',used:1},
 {name:'av1-1440p60',codec:'AV1',res:'2560×1440',fps:60,br:35,floor:12,enc:'NVENC · VA-API',browser:'Chrome 116+',used:1},
 {name:'av1-1080p120',codec:'AV1',res:'1920×1080',fps:120,br:28,floor:10,enc:'NVENC',browser:'Chrome 116+',used:1},
 {name:'hevc-2160p60',codec:'HEVC',res:'3840×2160',fps:60,br:40,floor:16,enc:'NVENC · VA-API',browser:'Safari 17+',used:1},
 {name:'hevc-1440p60',codec:'HEVC',res:'2560×1440',fps:60,br:30,floor:11,enc:'NVENC · VA-API',browser:'Safari 17+',used:2},
 {name:'hevc-1080p120',codec:'HEVC',res:'1920×1080',fps:120,br:25,floor:9,enc:'NVENC',browser:'Safari 17+',used:1},
 {name:'h264-1440p60',codec:'H.264',res:'2560×1440',fps:60,br:24,floor:8,enc:'universal',browser:'all',used:1},
 {name:'h264-1080p60',codec:'H.264',res:'1920×1080',fps:60,br:18,floor:6,enc:'universal',browser:'all',used:3}];
const ALERTS=[
 {sev:'err',title:'quasar-node-5 is degraded',body:'GPU enumeration returned 0 devices · heartbeat 41s late',age:'1h 5m',cta:'Open host',go:'#/fleet/hosts/d0e13b77'},
 {sev:'err',title:'quasar-node-6 offline',body:'No heartbeat for 14 minutes · 0 sessions affected',age:'14m',cta:'Open host',go:'#/fleet/hosts/2b6690af'},
 {sev:'warn',title:'1 session degraded',body:'tobi · Helldivers 2 · 68 ms latency, dropping to H.264',age:'22m',cta:'Open session',go:'#/sessions/s-4e2ac015'},
 {sev:'warn',title:'Storage below 10% on quasar-node-5',body:'12 GB free of 240 GB · new homes will fail to provision',age:'3h',cta:'Open storage',go:'#/fleet/storage'}];
window.QDATA={HOSTS,SESSIONS,APPS,USERS,INVITES,AUDIT,STORAGE,IMAGES,PRESETS,LAUNCH,STREAMP,ALERTS};
