const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const A=require('./web/continuity.js');
const M=require('./web/metrics.js');

// Run production page functions in a DOM-free harness. Snapshot tests hold
// media evidence passing to expose a missing required agent verdict.
function page(search='',renderPage=false,status) {
 const element=()=>({value:'pattern',replaceChildren(){},append(){},click(){},textContent:''});
 const elements=new Map();
 const context={window:{relaisMetrics:M,relaisContinuity:A,addEventListener(){}},document:{getElementById(id){if(!elements.has(id))elements.set(id,element());return elements.get(id);},createElement:element,addEventListener(){},removeEventListener(){},hidden:false},location:{search,hash:''},URLSearchParams,Headers,Blob,URL:{createObjectURL(blob){context.savedBlob=blob;return 'blob:test';},revokeObjectURL(){}},performance:{now:()=>10000,timeOrigin:0},setInterval(){},clearInterval(){},clearTimeout(){},setTimeout(fn){fn();},fetch:async()=>{if(status)return {ok:true,text:async()=>JSON.stringify(status)};throw new Error('status unavailable');}};
 const source=fs.readFileSync(require.resolve('./web/demo.js'),'utf8');
 const instrument=`
 window.testPage={setCall(s){call=s;},snapshot,event,start,saveResults};
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
  context.fetch=async()=>({ok:true,text:async()=>JSON.stringify({selected_agent:'echo',relayBuffer:mode})});
  await assert.rejects(api.start(),/media started/);
  assert.equal(context.window.sourceAcquired,true);
  assert.equal(context.window.relaisDemo.results().relayBuffer,mode);
  // Later diagnostics must not relabel the call if the server is restarted.
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
