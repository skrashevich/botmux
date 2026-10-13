/* Telegram presentation and keyboard model. No DOM or network dependencies. */
(function(root){
'use strict';
const esc=v=>String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const clone=v=>JSON.parse(JSON.stringify(v));
const actions=['callback_data','url','web_app','login_url','switch_inline_query','switch_inline_query_current_chat','switch_inline_query_chosen_chat','copy_text','callback_game','pay','disabled'];
const action=b=>actions.find(k=>b[k]!=null)||'';
function safeURL(value,httpsOnly=false){try{if(typeof value!=='string'||! /^(https?|tg):\/\//i.test(value))return '';const u=new URL(value);return (httpsOnly?u.protocol==='https:':['http:','https:','tg:'].includes(u.protocol))?u.href:'';}catch{return '';}}
function validateKeyboard(kb){
 const errors=[];
 if(!kb||!Array.isArray(kb.inline_keyboard))return ['inline_keyboard must be an array'];
 if(kb.force_reply!=null&&typeof kb.force_reply!=='boolean')errors.push('force_reply must be boolean');
 kb.inline_keyboard.forEach((row,r)=>{
  if(!Array.isArray(row)){errors.push(`Row ${r+1} must be an array`);return;}
  row.forEach((b,c)=>{
   const fail=s=>errors.push(`${r+1}:${c+1} — ${s}`);
   if(!b||typeof b!=='object'||Array.isArray(b)){fail('button must be an object');return;}
   if(typeof b.text!=='string'||!b.text.trim())fail('text is required');
   const keys=actions.filter(k=>b[k]!=null);if(keys.length!==1){fail('choose exactly one action');return;}
   const k=keys[0],v=b[k];
   if(b.style&&!['primary','success','danger'].includes(b.style))fail('invalid style');
   if(b.icon_custom_emoji_id!=null&&(typeof b.icon_custom_emoji_id!=='string'||!/^\d+$/.test(b.icon_custom_emoji_id)))fail('emoji ID must contain digits');
   if(k==='callback_data'&&(typeof v!=='string'||new TextEncoder().encode(v).length<1||new TextEncoder().encode(v).length>64))fail('callback data must contain 1–64 UTF-8 bytes');
   if(['switch_inline_query','switch_inline_query_current_chat'].includes(k)&&typeof v!=='string')fail('query must be text');
   if(k==='url'&&(typeof v!=='string'||!safeURL(v)))fail('use an HTTP, HTTPS or tg:// URL');
   if(['web_app','login_url','copy_text','callback_game','disabled','switch_inline_query_chosen_chat'].includes(k)&&(!v||typeof v!=='object'||Array.isArray(v)))fail('action parameters must be an object');
   if(['web_app','login_url'].includes(k)&&!safeURL(v?.url,true))fail('HTTPS URL is required');
   if(k==='copy_text'&&(typeof v?.text!=='string'||[...v.text].length<1||[...v.text].length>256))fail('copy text must contain 1–256 characters');
   if(k==='switch_inline_query_chosen_chat'){
    if(v.query!=null&&typeof v.query!=='string')fail('query must be text');
    for(const key of ['allow_user_chats','allow_bot_chats','allow_group_chats','allow_channel_chats'])if(v[key]!=null&&typeof v[key]!=='boolean')fail(`${key} must be boolean`);
   }
   if(k==='login_url'){
    for(const key of ['forward_text','bot_username'])if(v[key]!=null&&typeof v[key]!=='string')fail(`${key} must be text`);
    if(v.request_write_access!=null&&typeof v.request_write_access!=='boolean')fail('request_write_access must be boolean');
   }
   if(k==='pay'&&(v!==true||r!==0||c!==0))fail('payment must be the first button');
   if(k==='callback_game'&&(r!==0||c!==0))fail('game must be the first button');
  });
 });return errors;
}
function moveButton(kb,fromRow,fromCol,toRow,toCol){
 const next=clone(kb);if(!next.inline_keyboard[fromRow]?.[fromCol]||!next.inline_keyboard[toRow])throw Error('invalid button position');
 const [b]=next.inline_keyboard[fromRow].splice(fromCol,1);next.inline_keyboard[toRow].splice(toCol,0,b);return next;
}
function changeAction(button,key){
 if(!actions.includes(key))throw Error('unknown action');const b=clone(button);
 actions.forEach(k=>delete b[k]);
 b[key]=['url','callback_data','switch_inline_query','switch_inline_query_current_chat'].includes(key)?'':key==='pay'?true:key==='copy_text'?{text:''}:['web_app','login_url'].includes(key)?{url:''}:{};
 return b;
}
function groupMessages(messages){
 const sorted=[...messages].sort((a,b)=>a.date-b.date||a.id-b.id),result=[],groups=new Map();
 for(const m of sorted){
  const key=m.media_group_id?JSON.stringify([m.bot_id,m.chat_id,m.message_thread_id||0,m.media_group_id]):null;
  if(key){let g=groups.get(key);if(!g){g={key,messages:[]};groups.set(key,g);result.push(g);}g.messages.push(m);}else result.push({key:null,messages:[m]});
 }return result;
}
function label(v,depth=0){if(depth>64)return ''; if(typeof v==='string')return v;if(Array.isArray(v))return v.map(x=>label(x,depth+1)).join('');if(!v||typeof v!=='object')return '';return v.alternative_text||label(v.text,depth+1)||'';}
function emoji(id,alt,ctx){return `<span class="tg-emoji-wrap"><img class="tg-custom-emoji" src="/api/emoji?bot_id=${encodeURIComponent(ctx.botID)}&id=${encodeURIComponent(id)}" alt="${esc(alt||'◈')}" loading="lazy" onerror="this.replaceWith(document.createTextNode(this.alt))"></span>`;}
function keyboard(kb,ctx={}){
 if(!Array.isArray(kb?.inline_keyboard))return '';
 const l=ctx.label||((ru,en)=>en);
 return `<div class="telegram-keyboard">${kb.inline_keyboard.filter(Array.isArray).map(row=>`<div class="telegram-button-row">${row.filter(b=>b&&typeof b==='object').map(b=>{
 const text=typeof b.text==='string'?esc(b.text):richText(b.text,ctx);
 const icon=b.icon_custom_emoji_id?emoji(b.icon_custom_emoji_id,'◈',ctx):'';
 const style=['primary','success','danger','link'].includes(b.style)?b.style:'';
 const k=action(b),title=k==='callback_data'?`callback: ${b.callback_data}`:k==='disabled'?l('Недоступна','Disabled'):k==='copy_text'?l('Копировать','Copy'):k;
 if(k==='url'&&safeURL(b.url))return `<a class="btn btn-sm ${style}" href="${esc(safeURL(b.url))}" target="_blank" rel="noopener noreferrer">${icon}${text}</a>`;
 if(k==='copy_text')return `<button type="button" class="btn btn-sm ${style}" data-copy-text="${esc(b.copy_text.text)}" onclick="copyTelegramText(this)">${icon}${text}</button>`;
 if(k==='disabled')return `<button type="button" class="btn btn-sm ${style}" disabled>${icon}${text}</button>`;
 return `<span class="btn btn-sm ${style}" role="img" title="${esc(title)}" aria-label="${esc(title)}">${icon}${text}</span>`;
 }).join('')}</div>`).join('')}</div>`;
}
function math(expression,display,ctx){
 try{const katex=Object.hasOwn(ctx,'katex')?ctx.katex:root.katex;if(katex)return katex.renderToString(String(expression||''),{displayMode:display,throwOnError:false,trust:false,strict:'warn',maxExpand:1000,maxSize:20});}catch{}
 return `<code class="rich-math-fallback" data-expression="${esc(expression)}" data-display="${!!display}">${esc(expression)}</code>`;
}
function richText(v,ctx={},depth=0){
 if(depth>64)return '';
 if(typeof v==='string')return esc(v).replace(/\n/g,'<br>');if(Array.isArray(v))return v.map(x=>richText(x,ctx,depth+1)).join('');if(!v||typeof v!=='object')return '';
 const t=richText(v.text,ctx,depth+1),l=ctx.label||((ru,en)=>en),prefix=ctx.prefix||'rich';
 const anchor=(kind,name)=>`${prefix}-${kind}-${encodeURIComponent(name||'')}`;
 const link=(url,text=t)=>url?`<a href="${esc(url)}" target="_blank" rel="noopener noreferrer">${text}</a>`:text;
 const tags={bold:'strong',italic:'em',underline:'u',strikethrough:'s',subscript:'sub',superscript:'sup',marked:'mark',code:'code'};
 if(tags[v.type])return `<${tags[v.type]}>${t}</${tags[v.type]}>`;
 switch(v.type){
 case 'spoiler':return `<span class="rich-spoiler" role="button" tabindex="0" aria-expanded="false" aria-label="${esc(l('Показать скрытый текст','Reveal spoiler'))}">${t}</span>`;
 case 'url':return link(safeURL(v.url));
 case 'custom_emoji':return emoji(v.custom_emoji_id,v.alternative_text,ctx);
 case 'mathematical_expression':return math(v.expression,false,ctx);
 case 'date_time':{const date=new Date(Number(v.unix_time)*1000);return `<time datetime="${Number.isFinite(date.getTime())?esc(date.toISOString()):''}" title="${esc(v.date_time_format)}">${t}</time>`;}
 case 'text_mention':return link(`tg://user?id=${encodeURIComponent(v.user?.id||'')}`,t||esc(v.user?.first_name));
 case 'mention':return link(/^@?\w+$/.test(v.username)?'https://t.me/'+v.username.replace(/^@/,''):'',t||esc(v.username));
 case 'email_address':return link(/^[^\s<>]+@[^\s<>]+$/.test(v.email_address)?'mailto:'+encodeURIComponent(v.email_address):'',t||esc(v.email_address));
 case 'phone_number':return link(/^[+\d ()-]+$/.test(v.phone_number)?'tel:'+encodeURIComponent(v.phone_number):'',t||esc(v.phone_number));
 case 'bank_card_number':return `<span class="rich-card">${t||esc(v.bank_card_number)}</span>`;
 case 'hashtag':case 'cashtag':case 'bot_command':return `<span class="rich-entity">${t||esc(v.hashtag||v.cashtag||v.bot_command)}</span>`;
 case 'button':return keyboard({inline_keyboard:[[v.button]]},ctx);
 case 'anchor':return `<span id="${esc(anchor('anchor',v.name))}"></span>`;
 case 'anchor_link':return `<a href="#${esc(anchor('anchor',v.anchor_name))}">${t}</a>`;
 case 'reference':return `<span id="${esc(anchor('reference',v.name))}" class="rich-reference">${t}</span>`;
 case 'reference_link':return `<a href="#${esc(anchor('reference',v.reference_name))}"><sup>${t||esc(v.reference_name)}</sup></a>`;
 default:return t||esc(v.alternative_text||'');
 }
}
function richMessage(rich,ctx={}){
 if(!Array.isArray(rich?.blocks))return '';
 const l=ctx.label||((ru,en)=>en),text=v=>richText(v,ctx),mediaURL=file=>`/api/media?bot_id=${encodeURIComponent(ctx.botID)}&file_id=${encodeURIComponent(file)}`;
 const caption=c=>c?`<figcaption>${text(c.text)}${c.credit?`<small>${text(c.credit)}</small>`:''}</figcaption>`:'';
 function blocks(list,depth=0){if(depth>48)return '';return (Array.isArray(list)?list:[]).map(b=>block(b,depth)).join('');}
 function block(b,depth){
 if(!b||typeof b!=='object')return '';
 switch(b.type){
 case 'paragraph':return `<p>${text(b.text)}</p>`;
 case 'section_heading':return `<h${Math.max(1,Math.min(6,Math.trunc(Number(b.size))||3))}>${text(b.text)}</h${Math.max(1,Math.min(6,Math.trunc(Number(b.size))||3))}>`;
 case 'preformatted':return `<pre data-language="${esc(b.language)}"><code>${text(b.text)}</code></pre>`;
 case 'footer':return `<footer>${text(b.text)}</footer>`;
 case 'divider':return '<hr>';
 case 'mathematical_expression':return `<div class="rich-math">${math(b.expression,true,ctx)}</div>`;
 case 'anchor':return text({type:'anchor',name:b.name});
 case 'list':return `<ul class="rich-list">${(b.items||[]).map(i=>`<li>${i.has_checkbox?`<input type="checkbox" disabled aria-label="${esc(l('Задача','Task'))}" ${i.is_checked?'checked':''}>`:''}${i.label?`<span class="rich-list-label">${esc(i.label)}</span>`:i.value!=null?`<span>${esc(i.value)}.</span>`:''}<div>${blocks(i.blocks,depth+1)}</div></li>`).join('')}</ul>`;
 case 'block_quotation':return `<blockquote>${blocks(b.blocks,depth+1)}${b.credit?`<cite>${text(b.credit)}</cite>`:''}</blockquote>`;
 case 'expandable_block_quotation':return `<details class="rich-quote"><summary>${esc(l('Цитата','Quotation'))}</summary><blockquote>${text(b.text)}${b.credit?`<cite>${text(b.credit)}</cite>`:''}</blockquote></details>`;
 case 'pull_quotation':return `<blockquote class="rich-pull">${text(b.text)}${b.credit?`<cite>${text(b.credit)}</cite>`:''}</blockquote>`;
 case 'collage':return `<figure><div class="rich-collage">${blocks(b.blocks,depth+1)}</div>${caption(b.caption)}</figure>`;
 case 'slideshow':return `<figure><div class="rich-slideshow" tabindex="0" aria-label="${esc(l('Слайды: прокрутите для просмотра','Slideshow: scroll to browse'))}">${blocks(b.blocks,depth+1)}</div>${caption(b.caption)}</figure>`;
 case 'table':return `<div class="rich-table-scroll"><table class="rich-table ${b.is_bordered?'bordered':''} ${b.is_striped?'striped':''} ${b.is_compact?'compact':''}">${b.caption?`<caption>${text(b.caption)}</caption>`:''}<tbody>${(b.cells||[]).map(row=>`<tr>${row.map(c=>{
 const tag=c.is_header?'th':'td',align=['left','center','right'].includes(c.align)?c.align:'left',valign=['top','middle','bottom'].includes(c.valign)?c.valign:'top';
 return `<${tag} colspan="${Math.max(1,Math.min(100,Number(c.colspan)||1))}" rowspan="${Math.max(1,Math.min(100,Number(c.rowspan)||1))}" style="text-align:${align};vertical-align:${valign}">${text(c.text)}</${tag}>`;
 }).join('')}</tr>`).join('')}</tbody></table></div>`;
 case 'details':return `<details ${b.is_open?'open':''}><summary>${text(b.summary)}</summary>${blocks(b.blocks,depth+1)}</details>`;
 case 'buttons':return `<div class="rich-buttons" style="text-align:${['left','center','right'].includes(b.align)?b.align:'left'}">${keyboard({inline_keyboard:[b.buttons||[]]},ctx)}</div>`;
 case 'map':{
 const lat=Number(b.location?.latitude),lon=Number(b.location?.longitude);
 if(!Number.isFinite(lat)||!Number.isFinite(lon)||Math.abs(lat)>90||Math.abs(lon)>180)return `<p>${esc(l('Карта недоступна','Map unavailable'))}</p>`;
 const delta=180/2**Math.max(1,Math.min(18,Number(b.zoom)||12)),bbox=[Math.max(-180,lon-delta),Math.max(-85,lat-delta),Math.min(180,lon+delta),Math.min(85,lat+delta)].join(',');
 const url='https://www.openstreetmap.org/export/embed.html?bbox='+encodeURIComponent(bbox)+'&layer=mapnik&marker='+encodeURIComponent(lat+','+lon);
 return `<figure><iframe class="rich-map" title="${esc(l('Карта','Map'))}" src="${esc(url)}" loading="lazy" referrerpolicy="no-referrer"></iframe>${caption(b.caption)}</figure>`;
 }
 case 'thinking':return `<p class="rich-thinking" role="status">${text(b.text)||esc(l('Обработка…','Thinking…'))}</p>`;
 case 'photo':case 'video':case 'animation':case 'audio':case 'document':case 'voice_note':{
 let file=b[b.type];if(b.type==='photo')file=Array.isArray(file)?file.at(-1):file;
 const id=file?.file_id;if(!id)return `<p>${esc(l('Медиа недоступно','Media unavailable'))}</p>`;
 const src=mediaURL(id);let html;
 if(b.type==='photo')html=`<img src="${src}" loading="lazy" alt="${esc(label(b.caption?.text)||l('Фото','Photo'))}" onclick="openLightbox(this.src)">`;
 else if(['video','animation'].includes(b.type))html=`<video src="${src}" controls preload="metadata" playsinline ${b.type==='animation'?'loop muted':''}></video>`;
 else if(['audio','voice_note'].includes(b.type))html=`<audio src="${src}" controls preload="none"></audio>`;
 else html=`<a class="msg-media-link" href="${src}" target="_blank" rel="noopener noreferrer">📎 ${esc(file.file_name||l('Документ','Document'))}</a>`;
 if(b.has_spoiler)html=`<details class="rich-media-spoiler"><summary>${esc(l('Показать медиа','Reveal media'))}</summary>${html}</details>`;
 return `<figure>${html}${caption(b.caption)}</figure>`;
 }
 default:return `<details class="rich-unsupported"><summary>${esc(l('Неизвестный блок','Unknown block'))}: ${esc(b.type)}</summary><pre>${esc(JSON.stringify(b,null,2))}</pre></details>`;
 }
 }
 return `<div class="rich-message" dir="${rich.is_rtl?'rtl':'auto'}">${blocks(rich.blocks)}</div>`;
}
const api={actions,action,clone,esc,safeURL,validateKeyboard,moveButton,changeAction,groupMessages,label,keyboard,math,richText,richMessage};
root.TelegramUI=api;if(typeof module!=='undefined'&&module.exports)module.exports=api;
})(typeof globalThis!=='undefined'?globalThis:this);
