// Shared UI helpers: icons, small render primitives.
const I={
 overview:'<path d="M2.5 3.5h5v4h-5zM2.5 10h5v3h-5zM9.5 3.5h4v3h-4zM9.5 9h4v4h-4z"/>',
 sessions:'<path d="M1.5 8h3l2-4.5L9 12.5l1.8-4.5h3.7" stroke-linecap="round" stroke-linejoin="round"/>',
 fleet:'<rect x="2" y="2.5" width="12" height="4.5" rx="1.2"/><rect x="2" y="9" width="12" height="4.5" rx="1.2"/><path d="M4.6 4.75h.01M4.6 11.25h.01" stroke-linecap="round"/>',
 library:'<rect x="2" y="2.5" width="5" height="5" rx="1"/><rect x="9" y="2.5" width="5" height="5" rx="1"/><rect x="2" y="9" width="5" height="5" rx="1"/><rect x="9" y="9" width="5" height="5" rx="1"/>',
 streaming:'<path d="M2 11.5V8a6 6 0 0 1 12 0v3.5" /><rect x="1.5" y="9.5" width="3" height="4.5" rx="1.2"/><rect x="11.5" y="9.5" width="3" height="4.5" rx="1.2"/>',
 people:'<circle cx="6.2" cy="5.6" r="2.4"/><path d="M1.8 13.2a4.4 4.4 0 0 1 8.8 0" stroke-linecap="round"/><path d="M10.8 3.6a2.4 2.4 0 0 1 0 4.6M11.6 13.2a4.4 4.4 0 0 0-1.1-2.9" stroke-linecap="round"/>',
 audit:'<rect x="3" y="1.8" width="10" height="12.4" rx="1.6"/><path d="M5.6 5.4h4.8M5.6 8h4.8M5.6 10.6h2.8" stroke-linecap="round"/>',
 settings:'<circle cx="8" cy="8" r="2.2"/><path d="M8 1.6v1.6M8 12.8v1.6M1.6 8h1.6M12.8 8h1.6M3.5 3.5l1.1 1.1M11.4 11.4l1.1 1.1M12.5 3.5l-1.1 1.1M4.6 11.4l-1.1 1.1" stroke-linecap="round"/>',
 storage:'<ellipse cx="8" cy="4" rx="5.5" ry="2.2"/><path d="M2.5 4v8c0 1.2 2.5 2.2 5.5 2.2s5.5-1 5.5-2.2V4" /><path d="M2.5 8c0 1.2 2.5 2.2 5.5 2.2s5.5-1 5.5-2.2"/>',
 image:'<rect x="2" y="3" width="12" height="10" rx="1.6"/><circle cx="5.8" cy="6.6" r="1.1"/><path d="M2.4 11.4l3.4-3 3 2.4 2.2-1.8 2.6 2.2" stroke-linecap="round" stroke-linejoin="round"/>',
 search:'<circle cx="7" cy="7" r="4.4"/><path d="M10.4 10.4L14 14" stroke-linecap="round"/>',
 refresh:'<path d="M13.5 8a5.5 5.5 0 1 1-1.7-4M13.5 2v3.6H10" stroke-linecap="round" stroke-linejoin="round"/>',
 plus:'<path d="M8 3.2v9.6M3.2 8h9.6" stroke-linecap="round"/>',
 chev:'<path d="M6 3.5L10.5 8 6 12.5" stroke-linecap="round" stroke-linejoin="round"/>',
 chevD:'<path d="M4 6l4 4 4-4" stroke-linecap="round" stroke-linejoin="round"/>',
 back:'<path d="M10 3.5L5.5 8 10 12.5" stroke-linecap="round" stroke-linejoin="round"/>',
 dots:'<circle cx="3.4" cy="8" r="1.1" fill="currentColor" stroke="none"/><circle cx="8" cy="8" r="1.1" fill="currentColor" stroke="none"/><circle cx="12.6" cy="8" r="1.1" fill="currentColor" stroke="none"/>',
 sun:'<circle cx="8" cy="8" r="3.3"/><path d="M8 1v1.6M8 13.4V15M1 8h1.6M13.4 8H15M3.2 3.2l1.1 1.1M11.7 11.7l1.1 1.1M12.8 3.2l-1.1 1.1M4.3 11.7l-1.1 1.1" stroke-linecap="round"/>',
 moon:'<path d="M13.4 9.6A5.8 5.8 0 0 1 6.4 2.6a5.9 5.9 0 1 0 7 7z" stroke-linejoin="round"/>',
 alert:'<path d="M8 2.6l6 10.8H2z" stroke-linejoin="round"/><path d="M8 6.6v3M8 11.4h.01" stroke-linecap="round"/>',
 bolt:'<path d="M8.8 1.6L3.4 9h4L7.2 14.4 12.6 7h-4z" stroke-linejoin="round"/>',
 rail:'<path d="M2 4h12M2 8h12M2 12h12" stroke-linecap="round"/>',
 collapse:'<path d="M9.5 4L5.5 8l4 4" stroke-linecap="round" stroke-linejoin="round"/><path d="M13 3v10" stroke-linecap="round"/>',
 filter:'<path d="M2 4h12M4.5 8h7M6.8 12h2.4" stroke-linecap="round"/>',
 download:'<path d="M8 2.6v7.6M4.8 7.4L8 10.6l3.2-3.2" stroke-linecap="round" stroke-linejoin="round"/><path d="M2.8 12.8h10.4" stroke-linecap="round"/>',
 copy:'<rect x="5.4" y="5.4" width="8.2" height="8.2" rx="1.6"/><path d="M10.6 5.4V4a1.6 1.6 0 0 0-1.6-1.6H4a1.6 1.6 0 0 0-1.6 1.6v5a1.6 1.6 0 0 0 1.6 1.6h1.4" stroke-linecap="round"/>',
 check:'<path d="M3.2 8.4l3 3 6.6-7" stroke-linecap="round" stroke-linejoin="round"/>',
 trash:'<path d="M3.4 4.8h9.2M6.3 4.8V3.5h3.4v1.3M4.7 4.8l.6 8.3h5.4l.6-8.3" stroke-linecap="round" stroke-linejoin="round"/><path d="M6.9 7v4M9.1 7v4" stroke-linecap="round"/>',
 pin:'<path d="M6.3 2.4h3.4l-.5 4 2.2 2.3H4.6l2.2-2.3z" stroke-linejoin="round"/><path d="M8 8.7v4.9" stroke-linecap="round"/>',
 profile:'<circle cx="8" cy="5.6" r="2.6"/><path d="M2.8 13.5a5.2 5.2 0 0 1 10.4 0" stroke-linecap="round"/>',
 overlay:'<rect x="2" y="2.5" width="12" height="8.5" rx="1.5"/><rect x="8.5" y="4.3" width="4" height="2.6" rx=".6"/>',
 lock:'<rect x="3.2" y="7.3" width="9.6" height="6.2" rx="1.3"/><path d="M5.3 7.3V5.2a2.7 2.7 0 0 1 5.4 0v2.1" stroke-linecap="round"/>',
 devices:'<rect x="1.8" y="3" width="9" height="6.2" rx="1"/><path d="M1.3 11h10" stroke-linecap="round"/><rect x="10.5" y="5.3" width="4.2" height="7.4" rx="1"/>',
 back2:'<path d="M12.5 8H3.5M7 4.5L3.5 8 7 11.5" stroke-linecap="round" stroke-linejoin="round"/>'
};
const icon=(n,c='')=>`<svg class="${c}" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">${I[n]||''}</svg>`;
const esc=s=>String(s).replace(/[&<>"]/g,m=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[m]));
const pct=(a,b)=>b?Math.round(a/b*100):0;
function spark(vals,color){
 const max=Math.max(...vals,1),min=0,w=74,h=20;
 const pts=vals.map((v,i)=>`${(i/(vals.length-1)*w).toFixed(1)},${(h-((v-min)/(max-min||1))*h).toFixed(1)}`).join(' ');
 return `<svg class="spark" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none"><polyline points="${pts}" fill="none" stroke="${color}" stroke-width="1.6" stroke-linejoin="round" stroke-linecap="round"/></svg>`;
}
const STATE_CHIP={online:'success',streaming:'success',active:'success',ready:'success',draining:'warning',connecting:'info',pulling:'info',pending:'warning','pending cleanup':'warning',stale:'warning',absent:'neutral',degraded:'danger',offline:'danger',failed:'danger',disabled:'neutral',ended:'neutral',expired:'neutral',revoked:'neutral',redeemed:'info'};
const chip=(label,kind)=>{const k=kind||STATE_CHIP[String(label).toLowerCase()]||'neutral';return `<span class="chip${k==='neutral'?'':' chip-'+k}">${k==='success'?'<i class="dot"></i>':''}${esc(label)}</span>`;};
const sdot=(s,k)=>{const t=k||STATE_CHIP[String(s).toLowerCase()]||'neutral';return `<i class="sdot ${t==='success'?'ok':t==='warning'?'warn':t==='danger'?'bad':t==='info'?'info':'off'}" title="${esc(s)}"></i>`;};
function bar(v,total,tone){const p=pct(v,total);return `<span class="bar ${tone||''}"><i style="width:${p}%"></i></span>`;}
function gauge(p,color){return `<span class="gauge" style="--p:${p};--gc:${color}"><span>${p}%</span></span>`;}
function tone(p){return p>=90?'danger':p>=75?'warning':'success'}
function toneC(p){return p>=90?'var(--danger)':p>=75?'var(--warning)':'var(--accent)'}
function head(title,sub,acts,crumbs){
 return `${crumbs?`<div class="crumbs">${crumbs}</div>`:''}<div class="page-head"><div><h1>${title}</h1>${sub?`<div class="sub">${sub}</div>`:''}</div><div class="acts">${acts||''}</div></div>`;
}
function tabs(items,active){
 return `<div class="tabs">${items.map(t=>`<button class="tab${t.id===active?' active':''}" onclick="go('${t.go}')">${t.label}${t.count!=null?`<span class="cnt">${t.count}</span>`:''}</button>`).join('')}</div>`;
}
function tableCard(cols,rows){
 return `<div class="card table-wrap"><table class="qtable"><thead><tr>${cols.map(c=>`<th class="${c.a||''}"${c.w?` style="width:${c.w}"`:''}>${c.l}</th>`).join('')}</tr></thead><tbody>${rows}</tbody></table></div>`;
}
function menu(items){
 const id='m'+Math.random().toString(36).slice(2,8);
 return `<span class="menu-wrap"><button class="icon-btn" onclick="toggleMenu(event,'${id}')">${icon('dots')}</button>
 <div class="menu" id="${id}">${items.map(it=>it==='-'?'<hr>':`<button class="${it.danger?'danger':''}"${it.fn?` onclick="${it.fn}"`:''}>${it.label}</button>`).join('')}</div></span>`;
}
function toggleMenu(e,id){e.stopPropagation();const el=document.getElementById(id);const open=el.classList.contains('open');
 document.querySelectorAll('.menu.open').forEach(m=>{m.classList.remove('open');m.style.cssText='';});
 if(open)return;
 const r=e.currentTarget.getBoundingClientRect();
 document.body.appendChild(el);
 el.classList.add('open');
 el.style.position='fixed';el.style.top='';el.style.left='';el.style.right='';
 const h=el.offsetHeight,w=el.offsetWidth;
 const below=window.innerHeight-r.bottom>h+12;
 el.style.top=(below?r.bottom+6:r.top-h-6)+'px';
 el.style.left=Math.max(8,Math.min(r.right-w,window.innerWidth-w-8))+'px';}
document.addEventListener('click',()=>document.querySelectorAll('.menu.open').forEach(m=>{m.classList.remove('open');m.style.cssText='';}));
Object.assign(window,{I,icon,esc,pct,spark,chip,sdot,bar,gauge,tone,toneC,head,tabs,tableCard,menu,toggleMenu,STATE_CHIP});
