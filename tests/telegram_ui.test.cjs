const test=require('node:test'),assert=require('node:assert/strict');
const ui=require('../internal/server/templates/assets/telegram-ui.js');
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto');
const template=fs.readFileSync(path.join(__dirname,'../internal/server/templates/index.html'),'utf8');
const ctx={botID:7,prefix:'m42',katex:null};
const cdnAsset=extension=>{
 const tag=template.match(new RegExp('<(?:script|link)\\b[^>]*katex\\.min\\.'+extension+'[^>]*>'))?.[0];assert.ok(tag);
 return {tag,url:tag.match(/(?:src|href)="([^"]+)"/)[1],integrity:tag.match(/integrity="([^"]+)"/)[1]};
};
async function verifiedCDNAsset(asset){
 const cache=path.join(require('node:os').tmpdir(),'botmux-test-'+crypto.createHash('sha256').update(asset.url+asset.integrity).digest('hex'));
 const matches=bytes=>'sha384-'+crypto.createHash('sha384').update(bytes).digest('base64')===asset.integrity;
 let bytes;try{bytes=fs.readFileSync(cache);}catch(error){if(error.code!=='ENOENT')throw error;}
 if(bytes&&matches(bytes))return bytes;
 const response=await fetch(asset.url,{signal:AbortSignal.timeout(15000)});assert.ok(response.ok,asset.url);
 bytes=Buffer.from(await response.arrayBuffer());assert.ok(matches(bytes),'CDN asset integrity mismatch');fs.writeFileSync(cache,bytes);return bytes;
}
test.before(async()=>{
 const [script]=await Promise.all([verifiedCDNAsset(cdnAsset('js')),verifiedCDNAsset(cdnAsset('css'))]);
 const module={exports:{}};require('node:vm').runInNewContext(script.toString('utf8'),{module,exports:module.exports,console},{timeout:1000});ctx.katex=module.exports;
});
const button=action=>({text:'Action',...action});

test('every inline action validates and retains nested options',()=>{
 const valid=[{callback_data:'я'.repeat(32)},{url:'tg://user?id=1'},{web_app:{url:'https://example.com'}},{login_url:{url:'https://example.com',forward_text:'Login',bot_username:'loginbot',request_write_access:true}},{switch_inline_query:''},{switch_inline_query_current_chat:''},{switch_inline_query_chosen_chat:{query:'test',allow_group_chats:true}},{copy_text:{text:'copy'}},{callback_game:{}},{pay:true},{disabled:{}}];
 for(const action of valid){const kb={inline_keyboard:[[button(action)]],force_reply:true};assert.deepEqual(ui.validateKeyboard(kb),[]);assert.deepEqual(ui.clone(kb),kb);}
});
test('invalid keyboard payloads and UTF-8 byte limits fail before sending',()=>{
 for(const action of [{callback_data:'я'.repeat(33)},{callback_data:''},{callback_data:1},{url:'javascript:alert(1)'},{web_app:{url:'http://example.com'}},{copy_text:{text:'x'.repeat(257)}},{disabled:false},{pay:false},{login_url:{url:'https://example.com',request_write_access:'yes'}}])assert.ok(ui.validateKeyboard({inline_keyboard:[[button(action)]]}).length);
 assert.ok(ui.validateKeyboard({inline_keyboard:[[button({callback_data:'a',url:'https://example.com'})]]}).length);
 assert.ok(ui.validateKeyboard({inline_keyboard:[[button({callback_data:'a'})],[button({pay:true})]]}).length);
});
test('changing action preserves presentation and removes obsolete actions',()=>{
 const b={text:'Confirm',callback_data:'yes',style:'success',icon_custom_emoji_id:'123',future_option:{value:1}};
 const next=ui.changeAction(b,'login_url');assert.equal(next.callback_data,undefined);assert.equal(next.style,'success');assert.deepEqual(next.future_option,{value:1});assert.deepEqual(next.login_url,{url:''});assert.equal(b.callback_data,'yes');
});
test('reordering within and across rows is lossless and does not mutate drafts',()=>{
 const kb={inline_keyboard:[[button({callback_data:'A'}),button({copy_text:{text:'B'}})],[button({url:'https://example.com'})]],force_reply:true};
 const same=ui.moveButton(kb,0,0,0,1);assert.equal(same.inline_keyboard[0][1].callback_data,'A');
 const across=ui.moveButton(kb,0,1,1,0);assert.deepEqual(across.inline_keyboard[1][0].copy_text,{text:'B'});assert.equal(across.force_reply,true);assert.equal(kb.inline_keyboard[0].length,2);
});
test('albums coalesce noncontiguous and overlapping pages while retaining topic/bot isolation',()=>{
 const m=(id,bot=1,chat=2,topic=0)=>({id,bot_id:bot,chat_id:chat,message_thread_id:topic,media_group_id:'album',date:10});
 const merged=new Map([m(3),m(1),{...m(2),media_group_id:''},m(3),m(4,2),m(5,1,3),m(6,1,2,9)].map(x=>[`${x.bot_id}:${x.chat_id}:${x.id}`,x]));
 const groups=ui.groupMessages([...merged.values()]);assert.equal(groups.length,5);assert.deepEqual(groups[0].messages.map(x=>x.id),[1,3]);assert.equal(groups[1].key,null);
});
test('keyboard rendering escapes labels and never turns native callbacks into browser actions',()=>{
 const html=ui.keyboard({inline_keyboard:[[button({callback_data:'confirm',text:'<script>bad</script>'}),button({copy_text:{text:'"<img>'}}),button({url:'javascript:bad'})]]},ctx);
 assert.ok(html.includes('&lt;script&gt;'));assert.ok(!html.includes('href="javascript:'));assert.ok(!html.includes('onclick="confirm'));assert.ok(html.includes('data-copy-text="&quot;&lt;img&gt;"'));
});
test('all rich text entities render with formatting, scoped links and safe fallback',()=>{
 const entities=['bold','italic','underline','strikethrough','spoiler','subscript','superscript','marked','code','date_time','text_mention','url','email_address','phone_number','bank_card_number','mention','hashtag','cashtag','bot_command','anchor','anchor_link','reference','reference_link','custom_emoji','mathematical_expression','button'];
 for(const type of entities){const html=ui.richText({type,text:'Visible',url:'https://example.com',unix_time:123,date_time_format:'date',user:{id:1},email_address:'a@example.com',phone_number:'+123',username:'test',name:'ref',anchor_name:'ref',reference_name:'ref',custom_emoji_id:'123',alternative_text:'🙂',expression:'x^2',button:button({callback_data:'yes'})},ctx);assert.ok(html.length,type);}
 assert.match(ui.richText({type:'bold',text:[{type:'italic',text:'Nested'},' plain']},ctx),/<strong><em>Nested<\/em> plain<\/strong>/);
 assert.match(ui.richText({type:'date_time',text:'Bad date',unix_time:1e100},ctx),/Bad date/);
});
test('every Bot API 10.3 rich block renders visible structure',()=>{
 const blocks=[
 {type:'paragraph',text:'Body'},{type:'section_heading',size:2,text:'Heading'},{type:'preformatted',language:'go',text:'code()'},{type:'footer',text:'Footer'},{type:'divider'},{type:'mathematical_expression',expression:'\\frac{a}{b}'},{type:'anchor',name:'start'},
 {type:'list',items:[{label:'1.',has_checkbox:true,is_checked:true,blocks:[{type:'paragraph',text:'Task'}]}]},
 {type:'block_quotation',blocks:[{type:'paragraph',text:'Quote'}],credit:'Author'},{type:'expandable_block_quotation',text:'Hidden quote',credit:'Author'},{type:'pull_quotation',text:'Pull'},
 {type:'collage',blocks:[{type:'paragraph',text:'One'}],caption:{text:'Collage'}},{type:'slideshow',blocks:[{type:'paragraph',text:'Slide'}],caption:{text:'Slides'}},
 {type:'table',cells:[[{text:'Header',is_header:true,colspan:2},{text:'Value'}]],is_bordered:true,is_striped:true,is_compact:true,caption:'Table'},
 {type:'details',summary:'More',is_open:true,blocks:[{type:'paragraph',text:'Details'}]},
 {type:'map',location:{latitude:55.75,longitude:37.6},zoom:12},{type:'buttons',buttons:[button({url:'https://example.com'})],align:'right'},
 {type:'photo',photo:[{file_id:'photo'}],has_spoiler:true,caption:{text:'Photo',credit:'Credit'}},{type:'video',video:{file_id:'video'}},{type:'animation',animation:{file_id:'gif'}},{type:'audio',audio:{file_id:'audio'}},{type:'document',document:{file_id:'doc',file_name:'Report.pdf'}},{type:'voice_note',voice_note:{file_id:'voice'}},{type:'thinking',text:'Processing'}
 ];
 const html=ui.richMessage({blocks,is_rtl:true},ctx);assert.ok(!html.includes('Unknown block'));assert.match(html,/dir="rtl"/);assert.match(html,/<h2>Heading/);assert.match(html,/<table/);assert.match(html,/checked/);assert.match(html,/rich-slideshow/);assert.match(html,/katex/);assert.match(html,/file_id=photo/);assert.match(html,/Report.pdf/);
});
test('rich content cannot inject HTML, JavaScript URLs or unsafe TeX commands',()=>{
 const html=ui.richMessage({blocks:[{type:'paragraph',text:[{type:'url',url:'javascript:alert(1)',text:'<img onerror="bad">'},{type:'mathematical_expression',expression:'\\href{javascript:alert(1)}{Click}'}]},{type:'preformatted',language:'" onclick="bad',text:'<script>bad</script>'},{type:'future',payload:'<img src=x onerror=bad>'}]},ctx);
 assert.ok(!html.includes('<script>'));assert.ok(!html.includes('<img onerror='));assert.ok(!html.includes('href="javascript:'));assert.match(html,/&lt;script&gt;/);assert.match(html,/Unknown block/);
});
test('unsafe and malformed map coordinates are not embedded',()=>{assert.ok(!ui.richMessage({blocks:[{type:'map',location:{latitude:'<script>',longitude:0}}]},ctx).includes('<iframe'));});

test('unlabelled rich media and empty labels never recurse',()=>{
 assert.equal(ui.label(null),'');assert.equal(ui.label(undefined),'');assert.equal(ui.label({}),'');
 assert.match(ui.richMessage({blocks:[{type:'photo',photo:[{file_id:'p'}]}]},ctx),/alt="Photo"/);
 assert.equal(ui.safeURL('https:example.com'),'');
 assert.ok(ui.validateKeyboard({inline_keyboard:[[button({callback_data:'ok',icon_custom_emoji_id:123})]]}).length);
});

test('upload workflow restores controls and retains files not sent after a failure',async t=>{
 const fs=require('node:fs'),vm=require('node:vm');
 const html=fs.readFileSync(require('node:path').join(__dirname,'../internal/server/templates/index.html'),'utf8');
 const source=html.slice(html.indexOf('let attachmentItems='),html.indexOf("document.getElementById('attachmentInput').addEventListener"));
 for(const scenario of ['success','network_failure','history_failure'])await t.test(scenario,async()=>{
  const elements=new Map(),get=id=>{if(!elements.has(id))elements.set(id,{disabled:false,dataset:{},value:'',checked:false,style:{},focus(){}});return elements.get(id);};
  const controls=['uploadCancel','uploadSubmit','uploadThread','attachmentInput','caption'].map(get);
  let finish,fail;const request=new Promise((resolve,reject)=>{finish=resolve;fail=reject;});
  const sandbox=vm.createContext({document:{getElementById:get,querySelectorAll:()=>controls},TelegramUI:ui,esc:ui.esc,tgLabel:(ru,en)=>en,toast(){},loadMessages(){},URL:{revokeObjectURL(){}},replyToMsgID:0,currentBotID:1,request});
  vm.runInContext(source,sandbox);
  const pending=vm.runInContext(`attachmentItems=[{file:{name:'one.png',type:'image/png',size:2},type:'photo',caption:'',spoiler:false,url:'blob:one'}${scenario==='history_failure'?",{file:{name:'two.png',type:'image/png',size:2},type:'photo',caption:'',spoiler:false,url:'blob:two'}":''}];uploadRequest=()=>request;sendAttachments();`,sandbox);
  assert.ok(controls.every(x=>x.disabled),'controls must be frozen during upload');
  if(scenario==='success')finish([]);else{const error=new Error('failure');if(scenario==='history_failure')error.sentCount=1;fail(error);}
  await pending;
  assert.equal(get('uploadCancel').disabled,false,'Cancel must work after success or failure');
  assert.equal(get('uploadThread').disabled,false,'topic control must be restored');
  assert.equal(vm.runInContext('attachmentItems.length',sandbox),scenario==='success'?0:1,'keep only files not confirmed sent');
  if(scenario==='history_failure')assert.equal(vm.runInContext('attachmentItems[0].url',sandbox),'blob:two');
 });
});

test('KaTeX uses pinned integrity-checked CDN assets without vendored files',()=>{
 for(const extension of ['js','css']){
  const asset=cdnAsset(extension);assert.equal(asset.url,'https://cdn.jsdelivr.net/npm/katex@0.19.0/dist/katex.min.'+extension);
  assert.match(asset.integrity,/^sha384-[A-Za-z0-9+/]{64}$/);assert.match(asset.tag,/crossorigin="anonymous"/);assert.match(asset.tag,/referrerpolicy="no-referrer"/);
 }
 assert.match(cdnAsset('js').tag,/\basync\b/);assert.match(cdnAsset('css').tag,/media="print"/);
 assert.equal(fs.existsSync(path.join(__dirname,'../internal/server/templates/assets/vendor')),false);
 assert.equal(fs.existsSync(path.join(__dirname,'../scripts/update-katex.sh')),false);
});
test('CDN failure keeps TeX visible and escapes expression metadata',()=>{
 const expression='\\frac{a}{b}<img src=x onerror="bad">';
 const html=ui.math(expression,true,{katex:null});assert.match(html,/rich-math-fallback/);assert.match(html,/data-display="true"/);assert.ok(!html.includes('<img'));
 assert.match(html,/&lt;img/);assert.match(html,/&quot;bad&quot;/);
});
test('late CDN readiness upgrades pending formulas without replacing messages',()=>{
 const source=template.slice(template.indexOf('function renderTelegramMath(){'),template.indexOf("document.addEventListener('telegram-math-ready'"));
 const node={dataset:{expression:'x^2',display:'true'},outerHTML:'pending'};
 const sandbox=require('node:vm').createContext({telegramRenderContext:()=>ctx,TelegramUI:ui,document:{querySelectorAll:()=>[node]}});
 require('node:vm').runInContext(source+';renderTelegramMath();',sandbox);assert.match(node.outerHTML,/katex-display/);assert.ok(!node.outerHTML.includes('rich-math-fallback'));
});
test('CDN KaTeX renders inline and block fractions, radicals, integrals and matrices',()=>{
 for(const expression of ['\\frac{a^2+b^2}{c}','\\int_0^\\infty e^{-x^2}\\,dx=\\frac{\\sqrt{\\pi}}{2}','\\begin{pmatrix}1 & 2 \\\\ 3 & 4\\end{pmatrix}','\\sum_{n=1}^{\\infty}\\frac1{n^2}=\\frac{\\pi^2}{6}']){
  for(const html of [ui.richText({type:'mathematical_expression',expression},ctx),ui.richMessage({blocks:[{type:'mathematical_expression',expression}]},ctx)]){
   assert.match(html,/class="katex/);assert.ok(!html.includes('katex-error'),expression);assert.ok(!html.includes('rich-math-fallback'),expression);assert.match(html,/<math/);
  }
 }
});
