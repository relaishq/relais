const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const A=require('./web/continuity.js');
const M=require('./web/metrics.js');

// Run production page functions in a DOM-free harness. Snapshot tests hold
// media evidence passing to expose a missing required agent verdict.
function page(search='',renderPage=false,status,hash='',storage=new Map()) {
 const element=()=>({value:'pattern',replaceChildren(){},append(){},click(){},textContent:''});
 const elements=new Map();
 const context={window:{relaisMetrics:M,relaisContinuity:A,addEventListener(){}},document:{getElementById(id){if(!elements.has(id))elements.set(id,element());return elements.get(id);},createElement:element,addEventListener(){},removeEventListener(){},hidden:false},AbortController,sessionStorage:{getItem:key=>storage.get(key) || null,setItem:(key,value)=>storage.set(key,value)},location:{pathname:'/',search,hash},history:{replaceState(...args){context.replacedHistory=args;}},URLSearchParams,Headers,Blob,URL:{createObjectURL(blob){context.savedBlob=blob;return 'blob:test';},revokeObjectURL(){}},performance:{now:()=>10000,timeOrigin:0},setInterval(){},clearInterval(){},clearTimeout(){},setTimeout(fn,ms){if(ms===10000){context.uploadTimer=fn;return 10;}fn();},fetch:async()=>{if(status)return {ok:true,text:async()=>JSON.stringify(status)};throw new Error('status unavailable');}};
 const source=fs.readFileSync(require.resolve('./web/demo.js'),'utf8');
 const instrument=`
 window.testPage={setCall(s){call=s;},snapshot,event,start,saveResults,render,runChecklist,setChecklistSteps(){run=async()=>{};waitForLongHold=async()=>{};}};
 sample=async()=>{};
 observePage=()=>{};attach=()=>{};statuses=()=>{};samples=()=>{};
 sourceStream=async()=>{window.sourceAcquired=true;throw new Error("media started");};
 measurement=()=>({video:{maxMs:0},audio:{maxMs:0},videoDelta:{},audioDelta:{}});
 scheduleHold=()=>{};${renderPage?'':'render=()=>{};'}
 `;
 vm.runInNewContext(source.replace(/\}\)\(\);\s*$/,instrument+'})();'),context);
 return {api:context.window.testPage,context,elements};
}
function call() {
 return {agentMode:'demo',source:'pattern',id:'test',resource:'/calls/test',armed:true,readyAtMs:1,pc:{connectionState:'connected'},events:[],baselines:[],hold:{status:'pass',verdict:{status:'pass',reasons:[]}},hiddenPeriods:[],transitions:[],negotiations:[],descriptionChanges:[],restartCalls:[],sampleErrors:[],samples:[{atMs:10000,audio:{},video:{}}],latest:{atMs:13000,audio:{},video:{}},contentFrames:[],baselineCount:1,headroomRatio:0.5};
}
test('passing media cannot pass a demo event without its required agent verdict',()=>{
 const {api}=page();const s=call();
 s.events=[{kind:'move',windowVerdict:{status:'pass',reasons:[]}}];api.setCall(s);
 assert.equal(api.snapshot(s).events[0].verdict.status,'inconclusive');
 assert.equal(api.snapshot(s).agentMode,'demo');
});
test('event evaluates returned agent evidence even with failed status polling',async()=>{
 const {api,context}=page();const s=call();s.statusError='status unavailable';api.setCall(s);
 context.fetch=async(path)=>({ok:true,text:async()=>JSON.stringify(path.endsWith('/move')?{agent_continuity:{errors:['checkpoint unavailable']}}:{})});
 const event=await api.event('move');
 assert.equal(event.agentVerdict?.status,'inconclusive');
 assert.deepEqual(Array.from(event.agentVerdict.reasons),['checkpoint unavailable']);
});
test('agent selection failure prevents startup before acquiring source media',async()=>{
 const {api,context}=page();
 context.RTCPeerConnection=function(){return {close(){}};};
 await assert.rejects(api.start(),/status unavailable/);
 assert.equal(context.window.sourceAcquired,undefined);
});

test('echo preserves media verdict and unknown selection remains inconclusive',()=>{
 const {api}=page();const s=call();s.events=[{kind:'move',windowVerdict:{status:'pass',reasons:[]}}];api.setCall(s);
 s.agentMode='echo';assert.equal(api.snapshot(s).events[0].verdict.status,'pass');
 delete s.agentMode;assert.equal(api.snapshot(s).events[0].verdict.status,'inconclusive');
});
test('events cannot run before agent selection is established',async()=>{
 const {api}=page();const s=call();delete s.agentMode;api.setCall(s);
 await assert.rejects(api.event('move'),/selected agent/);
});
test('returned continuity evidence is evaluated independently of selected mode and polling',async()=>{
 const {api,context}=page();const s=call();s.agentMode='echo';s.statusError='status unavailable';api.setCall(s);
 context.fetch=async()=>({ok:true,text:async()=>JSON.stringify({agent_continuity:{errors:['source unavailable']}})});
 const event=await api.event('kill');assert.equal(event.agentVerdict?.status,'inconclusive');
});
test('startup rejects missing selected agent instead of acquiring media',async()=>{
 const {api,context}=page();context.RTCPeerConnection=function(){return {close(){}};};
 context.fetch=async()=>({ok:true,text:async()=>'{}'});
 await assert.rejects(api.start(),/selected agent missing or invalid/);
 assert.equal(context.window.sourceAcquired,undefined);
});

for(const mode of ['on','off']) {
 test(`saved JSON includes server buffer mode ${mode} pinned before media`,async()=>{
  const {api,context}=page('?relay-buffer='+ (mode==='on'?'off':'on'));
  context.RTCPeerConnection=function(){return {close(){}};};
  context.fetch=async()=>({ok:true,text:async()=>JSON.stringify({selected_agent:'echo',relayBuffer:mode,launchId:'test-launch'})});
  await assert.rejects(api.start(),/media started/);
  assert.equal(context.window.sourceAcquired,true);
  assert.equal(context.window.relaisDemo.results().relayBuffer,mode);
  // Later status diagnostics must not relabel the saved call.
  const s=call();s.agentMode='echo';s.relayBuffer=context.window.relaisDemo.results().relayBuffer;
  s.status={relayBuffer:mode==='on'?'off':'on'};api.setCall(s);
  const record=api.saveResults();
  assert.equal(record.relayBuffer,mode);
  assert.equal(JSON.parse(await context.savedBlob.text()).relayBuffer,mode);
 });
}
test('unknown buffer mode is unverified in saved JSON',async()=>{
 const {api,context}=page();api.setCall(call());api.saveResults();
 assert.equal(JSON.parse(await context.savedBlob.text()).relayBuffer,'unverified');
});
test('startup rejects missing buffer mode before acquiring source media',async()=>{
 const {api,context}=page();context.RTCPeerConnection=function(){return {close(){}};};
 context.fetch=async()=>({ok:true,text:async()=>JSON.stringify({selected_agent:'echo'})});
 await assert.rejects(api.start(),/relay buffer mode missing or invalid/);
 assert.equal(context.window.sourceAcquired,undefined);
});

test('visible buffer mode comes from status before a call starts',async()=>{
 const {elements}=page('?relay-buffer=on',true,{relayBuffer:'off'});
 await new Promise((resolve)=>setImmediate(resolve));
 assert.equal(elements.get('relay-buffer').textContent,'off');
});

for(const launchId of [undefined,'',42]) {
 test(`startup rejects invalid launch identity ${JSON.stringify(launchId)}`,async()=>{
  const {api,context}=page();context.RTCPeerConnection=function(){return {close(){}};};
  context.fetch=async()=>({ok:true,text:async()=>JSON.stringify({selected_agent:'echo',relayBuffer:'on',launchId})});
  await assert.rejects(api.start(),/demo launch identity missing or invalid/);
  assert.equal(context.window.sourceAcquired,undefined);
 });
}

test('token is removed from address bar and retained for results mutations',async()=>{
 const {api,context}=page('?source=camera',false,{attach_mode:true,launchId:'launch',results_enabled:true},'#token=launch-secret');
 await new Promise(setImmediate);
 assert.deepEqual(Array.from(context.replacedHistory),[null,'','/?source=camera']);
 const requests=[];
 context.fetch=async(path,options)=>{requests.push({path,options});return {ok:true,status:201};};
 api.setCall(call());api.saveResults();
 await Promise.resolve();
 assert.equal(requests.length,1);assert.equal(requests[0].path,'/demo/results');
 assert.equal(requests[0].options.headers.get('X-Relais-Demo-Token'),'launch-secret');
 assert.equal(JSON.parse(requests[0].options.body).version,5);
 assert.equal(JSON.parse(await context.savedBlob.text()).version,5);
 assert.equal(requests[0].options.body,await context.savedBlob.text());
});
test('checklist completion uploads its final snapshot and 404 disables further uploads',async()=>{
 const {api,context}=page('',false,{results_enabled:true});
 await new Promise(setImmediate);
 const requests=[];
 context.fetch=async(path,options)=>{requests.push({path,options});return {ok:false,status:404};};
 api.setCall(call());api.setChecklistSteps();
 const result=await api.runChecklist();
 assert.equal(result.running,false);assert.equal(requests.length,1);
 assert.equal(JSON.parse(requests[0].options.body).running,false);
 await new Promise(setImmediate);
 api.saveResults();await Promise.resolve();assert.equal(requests.length,1);
});
test('without results directory Save results downloads and skips upload',async()=>{
 const {api,context}=page();api.setCall(call());
 let requests=0;context.fetch=async()=>{requests++;throw new Error('unexpected upload');};
 api.saveResults();await Promise.resolve();assert.equal(requests,0);
 assert.equal(JSON.parse(await context.savedBlob.text()).version,5);
});
test('two-worker attached pool permits move and kill while drain is disabled',()=>{
 const {api,elements}=page('',true);const s=call();s.agentMode='echo';
 s.status={attach_mode:true,pool_size:2,expected_pool_size:2,workers:[{name:'a'},{name:'b'}]};
 api.setCall(s);api.render();
 assert.equal(api.snapshot(s).poolSize,2);
 assert.equal(elements.get('move').disabled,false);assert.equal(elements.get('kill').disabled,false);assert.equal(elements.get('drain').disabled,true);
});

test('attach reload restores tab token only for the same launch',async()=>{
 const storage=new Map();
 page('',false,{attach_mode:true,launchId:'same-launch'},'#token=tab-secret',storage);
 await new Promise(setImmediate);
 assert.equal(storage.get('relais-demo-token:same-launch'),'tab-secret');
 const {api,context}=page('',false,{attach_mode:true,launchId:'same-launch',results_enabled:true},'',storage);
 await new Promise(setImmediate);
 let header;
 context.fetch=async(path,options)=>{header=options.headers.get('X-Relais-Demo-Token');return {ok:true,status:201};};
 api.setCall(call());api.saveResults();await new Promise(setImmediate);
 assert.equal(header,'tab-secret');
 const restarted=page('',false,{attach_mode:true,launchId:'new-launch',results_enabled:true},'',storage);
 await new Promise(setImmediate);
 restarted.context.fetch=async(path,options)=>{header=options.headers.get('X-Relais-Demo-Token');return {ok:true,status:201};};
 restarted.api.setCall(call());restarted.api.saveResults();await new Promise(setImmediate);
 assert.equal(header,null);
});
test('local-network keeps fragment and never reads a stored attach token',async()=>{
 const storage=new Map([['relais-demo-token:launch','stored-secret']]);
 const {api,context}=page('',false,{attach_mode:false,launchId:'launch',results_enabled:true},'#token=lan-secret',storage);
 await new Promise(setImmediate);
 assert.equal(context.replacedHistory,undefined);
 assert.equal(storage.get('relais-demo-token:launch'),'stored-secret');
 context.fetch=async(path,options)=>{assert.equal(options.headers.get('X-Relais-Demo-Token'),'lan-secret');return {ok:true,status:201};};
 api.setCall(call());api.saveResults();await new Promise(setImmediate);
});
test('pending or failed results upload never blocks or changes checklist result',async()=>{
 for(const fail of [false,true]) {
  const {api,context,elements}=page('',false,{results_enabled:true});
  await new Promise(setImmediate);
  let signal;
  context.fetch=async(path,options)=>{signal=options.signal;if(fail)throw new Error('upload failed');return new Promise(()=>{});};
  api.setCall(call());api.setChecklistSteps();
  const result=await api.runChecklist();
  assert.equal(result.running,false);assert.equal(result.version,5);
  if(!fail){assert.equal(signal.aborted,false);context.uploadTimer();}
  await new Promise(setImmediate);
  assert.match(elements.get('error').textContent,fail?/upload failed/:/timed out/);
  if(!fail)assert.equal(signal.aborted,true);
  assert.equal(api.snapshot(call()).version,5);
 }
});
