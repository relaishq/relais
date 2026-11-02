'use strict';
const {test}=require('node:test');
const assert=require('node:assert/strict');
const {Controller}=require('./web/relay-change.js');
function fixture(overrides={}){
 let now=0,ended='',posts=[],offers=0;
 const handlers={},pc={iceConnectionState:'connected',iceGatheringState:'complete',signalingState:'stable',localDescription:{sdp:'offer'},restartIce(){},async createOffer(){offers++;return {type:'offer',sdp:'offer'};},async setLocalDescription(){},async setRemoteDescription(){},addEventListener(){},removeEventListener(){}};
 const stream={addEventListener(name,fn){handlers[name]=fn;},close(){this.closed=true;}};
 const c=new Controller({pc,path:'/calls/test',request:async(path)=>{posts.push(path);return {ok:true,text:async()=> 'answer'};},events:()=>stream,end:reason=>{ended=reason;},now:()=>now,sleep:async()=>{},tick:()=>1,untick:()=>{},...overrides});
 return {c,pc,posts,stream,handlers,get ended(){return ended;},get offers(){return offers;},time(n){now=n;}};
}
test('relay event makes one full restart on the existing peer connection and ignores replay',async()=>{
 const f=fixture();await f.c.restart({generation:1,deadline:new Date(Date.now()+10000).toISOString()});
 await f.c.restart({generation:1,deadline:new Date(Date.now()+10000).toISOString()});
 assert.deepEqual(f.posts,['/calls/test/ice-restart?generation=1']);assert.equal(f.offers,1);assert.equal(f.ended,'');
});
for(const mode of ['missing','throws'])test(`cannot restart (${mode}) ends the call with a reason`,async()=>{
 const f=fixture();f.c.open();f.pc.restartIce=mode==='missing'?undefined:()=>{throw Error('browser refused');};
 await f.c.restart({generation:1});assert.match(f.ended,/restart/i);assert.equal(f.posts.length,0);assert.equal(f.stream.closed,true);
});
test('terminal relay_change_failed closes events and ends the page call',()=>{
 const f=fixture();f.c.open();f.handlers.ended({data:JSON.stringify({reason:'relay_change_failed'})});assert.match(f.ended,/relay_change_failed/);assert.equal(f.stream.closed,true);
});
test('two seconds without inbound media requests a restart without generation; media progress resets the clock',async()=>{
 const f=fixture();f.c.observe([{type:'inbound-rtp',id:'audio',packetsReceived:2}]);
 f.time(1999);await f.c.check();assert.equal(f.posts.length,0);
 f.time(2000);await f.c.check();assert.deepEqual(f.posts,['/calls/test/ice-restart']);
 f.time(2500);f.c.observe([{type:'inbound-rtp',id:'audio',packetsReceived:3}]);
 f.time(4499);await f.c.check();assert.equal(f.posts.length,1);
});
test('disconnected ICE triggers caller recovery immediately',async()=>{
 const f=fixture();f.pc.iceConnectionState='disconnected';await f.c.check();assert.deepEqual(f.posts,['/calls/test/ice-restart']);
});
test('a refused offer stays available for a higher-generation rollback',async()=>{
 const f=fixture();f.c.request=async()=>({ok:false,status:409,text:async()=>'restart_mismatch'});
 await f.c.restart({generation:1});assert.equal(f.ended,'');
 f.pc.signalingState='have-local-offer';f.c.request=async path=>{f.posts.push(path);return {ok:true,text:async()=>'answer'};};
 await f.c.restart({generation:2});assert.equal(f.offers,1);assert.deepEqual(f.posts,['/calls/test/ice-restart?generation=2']);
});

function receiverSensitive(name,callback){
 return function(...args){
  if(this!==undefined)throw new TypeError(name+': Illegal invocation');
  return callback(...args);
 };
}
test('native interval runs without the controller as its receiver',()=>{
 let ticks=0;
 const f=fixture({tick:receiverSensitive('setInterval',()=>{ticks++;return 1;})});
 assert.doesNotThrow(()=>f.c.open());assert.equal(ticks,1);
});
test('native interval cleanup runs without the controller as its receiver',()=>{
 let clears=0;
 const f=fixture({untick:receiverSensitive('clearInterval',()=>{clears++;})});
 f.c.open();assert.doesNotThrow(()=>f.c.close());assert.equal(clears,1);
});
test('native fetch receives the restart offer without the controller as its receiver',async()=>{
 const posts=[];
 const f=fixture({request:receiverSensitive('fetch',async(path)=>{posts.push(path);return {ok:true,text:async()=>'answer'};})});
 await f.c.restart({generation:1});
 assert.deepEqual(posts,['/calls/test/ice-restart?generation=1']);assert.equal(f.ended,'');
});

for(const capable of [false,true])test(`Chrome gate samples receiver timestamps and declares restart capability ${capable}`,async()=>{
 const fs=require('node:fs'),vm=require('node:vm');
 const audio={type:'inbound-rtp',kind:'audio',ssrc:123,packetsReceived:4};
 const video={type:'inbound-rtp',kind:'video',framesDecoded:2};
 let rtp=4800;
 const receiver={track:{kind:'audio'},getSynchronizationSources(){assert.equal(this,receiver);return [{source:999,rtpTimestamp:99},{source:123,rtpTimestamp:rtp}];}};
 class Peer {
  constructor(){this.iceGatheringState='complete';this.localDescription={sdp:'offer'};}
  addEventListener(){} addTransceiver(){} getSenders(){return [];}
  async createOffer(){return {};}
  async setLocalDescription(){} async setRemoteDescription(){}
  getReceivers(){return [{track:{kind:'video'},getSynchronizationSources(){throw Error('wrong receiver');}},receiver];}
  async getStats(){return new Map([['audio',audio],['video',video]]);}
 }
 if(capable)Peer.prototype.restartIce=function(){};
 let now=1;
 let createHeaders;
 const timers=[],sandbox={RTCPeerConnection:Peer,navigator:{mediaDevices:{getUserMedia:async()=>({getTracks:()=>[]})}},fetch:async(path,options)=>{if(path==='/calls')createHeaders=options.headers;return {status:201,text:async()=>'answer',headers:{get:()=>'/calls/test'}};},relaisRelayChange:{Controller:class{open(){} observe(){}}},performance:{now:()=>now},setTimeout:fn=>timers.push(fn),console:{info(){},error(){}}};
 sandbox.window=sandbox;
 vm.runInNewContext(fs.readFileSync(require.resolve('../crash-run/chrome_page.js'),'utf8'),sandbox);
 for(let i=0;i<30 && !sandbox.chromeGate.calls[0].ready;i++)await Promise.resolve();
 const gate=sandbox.chromeGate.calls[0];
 assert.equal(gate.ready,true,JSON.stringify(gate.failure));
 assert.equal(createHeaders['Relais-Relay-Change'],capable?'true':undefined);
 assert.equal(!!gate.controller,capable);
 assert.equal(gate.samples[0].audio.rtpTimestamp,4800);
 assert.equal(gate.samples[0].sources.audioRTP,'RTCRtpReceiver.getSynchronizationSources().rtpTimestamp');
 assert.equal(gate.samples[0].sources.audioPackets,'RTCInboundRtpStreamStats.packetsReceived');
 assert.equal(gate.samples[0].sources.videoFrames,'RTCInboundRtpStreamStats.framesDecoded');
 rtp=9600;await timers.shift()();
 assert.equal(gate.samples[1].audio.rtpTimestamp,9600);
 receiver.getSynchronizationSources=()=>[{source:999,rtpTimestamp:12345}];await timers.shift()();
 assert.equal(gate.samples[2].audio.rtpTimestamp,undefined,'another SSRC cannot supply audio progress');
 assert.equal(gate.samples[2].sources.audioRTP,null);
 gate.samples=Array.from({length:3000},(_,at)=>({at,audio:{packetsReceived:at,rtpTimestamp:at*480,unrelatedStats:'omit this'},video:{framesDecoded:at}}));
 now=3000;
 const snapshot=sandbox.chromeGate.snapshot(0,2800);
 now+=234; // Simulate slow CDP serialization/transfer after the snapshot.
 assert.equal(snapshot.at,3000,'observation end must precede evidence transfer');
 assert.equal(snapshot.samples.length,200,'only the current window and its preceding sample cross CDP');
 assert.equal(snapshot.samples[0].at,2800);
 assert.equal(snapshot.samples[0].audio.packetsReceived,2800);
 assert.equal(snapshot.samples[0].audio.unrelatedStats,undefined);
 assert.equal(gate.samples.length,3000,'full diagnostics remain available');
 assert.ok(gate.timeline.some(e=>e.event==='createOffer-start'));
 assert.ok(gate.timeline.some(e=>e.event==='setRemoteDescription-done'));
 assert.equal(sandbox.chromeGate.setupState(0).ready,true);
 const originalFetch=sandbox.fetch;let release;
 sandbox.fetch=()=>new Promise(resolve=>{release=resolve;});
 const adding=sandbox.chromeGate.addCall();
 for(let i=0;i<30 && !release;i++)await Promise.resolve();
 const blocked=sandbox.chromeGate.setupState(1);
 assert.equal(blocked.index,1);
 assert.equal(blocked.ready,false);
 assert.equal(blocked.phase,'post-call','an unresolved call POST must remain diagnosable');
 assert.ok(blocked.phases.some(p=>p.phase==='gather-ice'));
 release(await originalFetch());await adding;
 const finished=sandbox.chromeGate.setupState(1);
 assert.equal(finished.ready,true);
 assert.equal(finished.phase,'sampling');
 assert.ok(finished.phases.some(p=>p.phase==='set-remote-description'));
});

test('event restart waits through its deadline before another stalled-media offer',async()=>{
 const f=fixture();f.c.observe([{type:'inbound-rtp',id:'audio',packetsReceived:1}]);
 await f.c.restart({generation:1,deadline:new Date(Date.now()+10000).toISOString()});
 f.time(2700);await f.c.check();f.time(9900);await f.c.check();
 assert.deepEqual(f.posts,['/calls/test/ice-restart?generation=1']);
 f.time(10100);await f.c.check();assert.equal(f.posts.length,2);
});
test('spontaneous restart allows two seconds after applying a delayed answer',async()=>{
 const f=fixture();f.pc.iceConnectionState='disconnected';
 f.c.request=async path=>{f.posts.push(path);f.time(5000);return {ok:true,text:async()=>'answer'};};
 f.pc.setRemoteDescription=async()=>f.time(6000);
 await f.c.check();f.time(7999);await f.c.check();assert.equal(f.posts.length,1);
 f.time(8000);await f.c.check();assert.equal(f.posts.length,2);
});
function eventFixture(){
 const streams=[];
 const f=fixture({events:()=>{
  const handlers={},stream={readyState:0,handlers,addEventListener(name,fn){handlers[name]=fn;},close(){this.closed=true;}};
  streams.push(stream);return stream;
 }});
 f.c.open();return {...f,streams};
}
test('closed EventSource reopens with capped backoff and resets it after open',async()=>{
 const f=eventFixture();let now=0;
 for(const delay of [50,100,200,400,800,1000,1000]){
  const stream=f.streams.at(-1),count=f.streams.length;stream.readyState=2;stream.handlers.error();
  f.time(now+delay-1);await f.c.check();assert.equal(f.streams.length,count);
  now+=delay;f.time(now);await f.c.check();assert.equal(f.streams.length,count+1);assert.equal(stream.closed,true);
 }
 const reopened=f.streams.at(-1);reopened.handlers.open();reopened.readyState=2;reopened.handlers.error();
 f.time(now+50);await f.c.check();assert.equal(f.streams.length,9);
});
test('native EventSource reconnect and close never create another stream',async()=>{
 const f=eventFixture(),stream=f.streams[0];stream.readyState=0;stream.handlers.error();
 f.time(10000);await f.c.check();assert.equal(f.streams.length,1);
 stream.readyState=2;stream.handlers.error();stream.handlers.error();
 f.c.close();f.time(20000);await f.c.check();assert.equal(f.streams.length,1);
});
test('single-relay unsupported restart rolls back and disables spontaneous retries',async()=>{
 const f=fixture();let rollbacks=0;
 f.pc.iceConnectionState='disconnected';
 f.pc.setLocalDescription=async d=>{if(d.type==='rollback'){rollbacks++;f.pc.signalingState='stable';}else f.pc.signalingState='have-local-offer';};
 f.c.request=async path=>{f.posts.push(path);return {ok:false,status:400,text:async()=>'invalid generation\n'};};
 await f.c.check();assert.equal(f.pc.signalingState,'stable');assert.equal(rollbacks,1);assert.equal(f.ended,'');
 f.time(100000);await f.c.check();assert.equal(f.posts.length,1);assert.equal(f.offers,1);
});
test('an invalid-offer 400 remains a refusal rather than disabling relay changes',async()=>{
 const f=fixture();let rollbacks=0;f.pc.iceConnectionState='disconnected';
 f.pc.setLocalDescription=async d=>{if(d.type==='rollback')rollbacks++;else f.pc.signalingState='have-local-offer';};
 f.c.request=async path=>{f.posts.push(path);return {ok:false,status:400,text:async()=>'invalid offer'};};
 await f.c.check();f.time(3000);await f.c.check();assert.equal(f.posts.length,2);assert.equal(rollbacks,0);
});

for(const capable of [false,true])test(`real demo startup declares restart capability ${capable} and keeps legacy calls`,async()=>{
 const fs=require('node:fs'),vm=require('node:vm');let headers,controllers=0;
 const element=()=>({addEventListener(){},value:'pattern',replaceChildren(){},append(){},click(){},textContent:''});
 class Peer {
  constructor(){this.connectionState='connected';this.iceGatheringState='complete';}
  addTransceiver(){} async createOffer(){return {type:'offer',sdp:'offer'};}
  async setLocalDescription(d){this.localDescription=d;} async setRemoteDescription(d){this.remoteDescription=d;}
  close(){this.connectionState='closed';}
 }
 if(capable)Peer.prototype.restartIce=function(){};
 const context={window:{relaisMetrics:require('./web/metrics.js'),relaisContinuity:require('./web/continuity.js'),relaisRelayChange:{Controller:class{constructor(){controllers++;}open(){}}},addEventListener(){}},
  RTCPeerConnection:Peer,document:{getElementById:element,createElement:element,addEventListener(){}},
  location:{search:'',hash:''},URLSearchParams,Headers,performance:{now:()=>10000},setInterval(){},clearInterval(){},
  fetch:async(path,options)=>{
   if(path==='/calls'){headers=options.headers;return {status:201,headers:{get:()=>'/calls/test'},text:async()=>'answer'};}
   return {ok:true,text:async()=>JSON.stringify({selected_agent:'echo',relayBuffer:'on',launchId:'test'})};
  }};
 const instrument=`observePage=()=>{};attach=()=>{};statuses=()=>{};samples=()=>{};render=()=>{};waitFor=async()=>{};
 sourceStream=async s=>{s.lastVideo=1;s.latest={audio:{packetsReceived:1}};return {getTracks:()=>[{kind:'audio'},{kind:'video'}]};};`;
 vm.runInNewContext(fs.readFileSync(require.resolve('./web/demo.js'),'utf8').replace(/\}\)\(\);\s*$/,instrument+'})();'),context);
 const result=await context.window.relaisDemo.start();
 assert.equal(headers.get('Relais-Relay-Change'),capable?'true':null);
 assert.equal(controllers,capable?1:0);assert.equal(result.state,'connected');
});
