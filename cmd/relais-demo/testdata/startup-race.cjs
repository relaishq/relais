// Go supplies real demo HTTP handlers. Only DOM and media APIs are doubled;
// production start, requests, readiness checks, cleanup, and saving run intact.
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const [base,scenario]=process.argv.slice(2);
let savedBlob,tracksStopped=0;
const element=()=>({addEventListener(){},value:'camera',replaceChildren(){},append(){},click(){},textContent:''});
const elements=new Map();
const context={window:{relaisMetrics:require('../web/metrics.js'),relaisContinuity:require('../web/continuity.js'),addEventListener(){}},
 document:{getElementById(id){if(!elements.has(id))elements.set(id,element());return elements.get(id);},createElement:element,addEventListener(){},removeEventListener(){},hidden:false},
 EventSource:class {addEventListener(){}close(){}},
 location:{search:'?source=camera',hash:''},URLSearchParams,Headers,Blob,
 URL:{createObjectURL(blob){savedBlob=blob;return 'blob:test';},revokeObjectURL(){}},
 performance:{now:()=>10000,timeOrigin:0},setInterval(){},clearInterval(){},clearTimeout(){},setTimeout(fn){fn();},
 fetch:(path,options)=>fetch(base+path,options),
 RTCPeerConnection:class {
  constructor(){this.connectionState='new';this.iceGatheringState='complete';}
  addEventListener(){}
  removeEventListener(){}
  addTransceiver(){}
  async createOffer(){return {type:'offer',sdp:'offer'};}
  async setLocalDescription(description){this.localDescription=description;if(scenario==='ice')await fetch(base+'/test/restart',{method:'POST'});}
  async setRemoteDescription(description){this.remoteDescription=description;this.connectionState='connected';}
  close(){this.connectionState='closed';}
 }};
const instrument=`
 observePage=()=>{};attach=()=>{};statuses=()=>{};samples=()=>{};render=()=>{};
 sourceStream=async(s)=>{
  s.owner='0';s.lastVideo=0;s.latest={audio:{packetsReceived:1}};
  if(${JSON.stringify(scenario)}==='camera')await fetch('/test/restart',{method:'POST'});
  const tracks=['audio','video'].map((kind)=>({kind,stop(){window.trackStopped();}}));
  return {getTracks:()=>tracks};
 };
 window.trackStopped=()=>{};
`;
vm.runInNewContext(fs.readFileSync(require.resolve('../web/relay-change.js'),'utf8'),context);
const source=fs.readFileSync(require.resolve('../web/demo.js'),'utf8');
vm.runInNewContext(source.replace(/\}\)\(\);\s*$/,instrument+'})();'),context);
context.window.trackStopped=()=>{tracksStopped++;};
(async()=>{
 const api=context.window.relaisDemo;
 let result,error;
 try {result=await api.start({source:'camera'});}catch(err){error=err;}
 if(scenario==='normal') {
  assert.ifError(error);
  assert.equal(result.state,'connected');
  assert.equal(result.relayBuffer,'on');
  const status=await (await fetch(base+'/demo/status')).json();
  assert.equal(result.launchId,status.launchId);
  api.saveResults();
  assert.equal(JSON.parse(await savedBlob.text()).relayBuffer,'on');
  await api.stop();
 } else {
  if(!error) {
   api.saveResults();
   const record=JSON.parse(await savedBlob.text());
   const status=await (await fetch(base+'/demo/status')).json();
   throw new Error('restart accepted a connected call under '+status.relayBuffer+' but saved relayBuffer='+record.relayBuffer);
  }
  assert.match(error.message,/POST \/calls 409: demo launch changed/);
  assert.equal(api.results().state,'ended');
  assert.equal(api.results().id,undefined);
  assert.equal(tracksStopped,2);
  assert.match(elements.get('error').textContent,/demo launch changed/);
 }
 console.log(scenario+': '+(error?'restart rejected before call creation':'same-launch call created and saved'));
})().catch((err)=>{console.error(err);process.exitCode=1;});
