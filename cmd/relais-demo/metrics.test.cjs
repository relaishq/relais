const {test}=require('node:test');
const assert=require('node:assert/strict');
const M=require('./web/metrics.js');
const row=(atMs,concealed=0,events=0,silent=0)=>({atMs,audio:{concealedSamples:concealed,silentConcealedSamples:silent,concealmentEvents:events,totalSamplesReceived:atMs*48,lastPacketReceivedTimestamp:atMs+1000}});
function event(kind='move',maxMs=80){return {kind,frameIntervalMs:1000/30,from:'a',to:'b',video:{method:'decoded counter',maxMs,countAfter:10},audio:{method:'total concealment samples',maxMs:Math.min(maxMs,80),lowerMs:Math.min(maxMs,80),exact:true,playoutCoverage:1},videoDelta:{framesDecoded:10},audioDelta:{packetsReceived:10},noiseGate:M.noiseGate({video:{count:5,maxMs:40},audio:{count:5,maxMs:0}}),connectionStateBefore:'connected',connectionStateAfter:'connected',stateChanges:[],iceRestarts:0,renegotiations:0,descriptionChanges:[]};}
test('open freeze and boundary interval cannot become a passing zero',()=>{
 assert.equal(M.gap([0,33,66],70,2070).maxMs,2004);
 assert.equal(M.gap([],70,2070).maxMs,'unverified');
 assert.deepEqual(M.gap([0,33,66,460,493],70,500),{maxMs:394,maxStartMs:66,maxEndMs:460,maxEnded:true,firstAfterMs:390,countAfter:2});
});
test('concealment audio is sample-exact despite irregular stats timestamps',()=>{
 const a=M.concealment([row(0),row(73,2400,1),row(181,3840,1),row(400,3840,1)],0,400,48000);
 assert.equal(a.maxMs,80);assert.equal(a.lowerMs,80);assert.equal(a.exact,true);assert.deepEqual(a.lastPacketReceivedTimestampDeltas,[73,108,219]);
 assert.equal(M.verdict({...event(),audio:a}).status,'pass');
 assert.equal(M.verdict({...event(),audio:M.concealment([row(0),row(300,5760,1)],0,300,48000)}).status,'fail');
});
test('total concealment drives the gap while non-silent samples remain diagnostic',()=>{
 const a=M.concealment([row(0),row(100,4800,1,3840)],0,100,48000);
 assert.equal(a.maxMs,100);assert.equal(a.nonSilentConcealedMs,20);assert.equal(a.concealedSamples,4800);assert.equal(M.verdict({...event(),audio:a}).status,'fail');
 assert.equal(M.verdict({...event(),audio:M.concealment([row(0),{atMs:100,audio:{}}],0,100,48000)}).status,'inconclusive');
 assert.equal(M.concealment([row(0,5),row(100,0)],0,100,48000).method,'sampling-only');
});
test('multiple concealment events yield bounds and cannot falsely fail',()=>{
 const a=M.concealment([row(0),row(200,7680,2)],0,200,48000);
 assert.equal(a.maxMs,160);assert.equal(a.lowerMs,80);assert.equal(a.exact,false);
 assert.equal(M.verdict({...event(),audio:a}).status,'inconclusive');
});
test('presentedFrames jump only downgrades a failing gap it overlaps',()=>{
 assert.equal(M.starved(20,22),true);assert.equal(M.starved(20,21),false);
 const e={...event('move',150),starved:true,video:{...event().video,maxMs:150,maxStartMs:100,maxEndMs:250},starvedPeriods:[{startMs:110,endMs:170}]};
 assert.equal(M.verdict(e).status,'inconclusive');
 assert.equal(M.verdict({...e,starvedPeriods:[{startMs:300,endMs:360}]}).status,'fail');
});
test('hidden overlap is invalid, including an unclosed hidden period',()=>{
 assert.equal(M.hiddenDuring([{startMs:10,endMs:20}],15,30),true);
 assert.equal(M.hiddenDuring([{startMs:10}],100,200),true);
 assert.equal(M.hiddenDuring([{startMs:10,endMs:20}],21,30),false);
 assert.deepEqual(M.verdict({...event(),hidden:true}),{status:'invalid',reasons:['page hidden']});
});
test('decoded content must advance: cached replay cannot count as recovery',()=>{
 const frames=[{atMs:0,counter:100,advanced:true},{atMs:33,counter:101,advanced:true},{atMs:70,...M.counterObservation(99,101)},{atMs:100,...M.counterObservation(101,101)},{atMs:300,...M.counterObservation(102,101)}];
 const r=M.content(frames,40,340,60,101);
 assert.equal(r.maxMs,267);assert.equal(r.firstNewContentMs,240);assert.equal(r.replayedOrOld,true);assert.equal(r.countAfter,1);
 assert.equal(M.content(frames.slice(0,4),40,2040,60,101).maxMs,2007);
 assert.equal(M.content(frames.slice(0,4),40,2040,60,101).firstNewContentMs,'unverified');
});
test('back-to-back windows clip lookback at previous end',()=>{
 assert.equal(M.windowStart(3000,2900),2900);assert.equal(M.windowStart(5000,2900),4000);
});
test('strict thresholds, connection and negotiation failures',()=>{
 assert.equal(M.verdict(event('move',100)).status,'fail');assert.equal(M.verdict(event('kill',100)).status,'pass');assert.equal(M.verdict({...event('kill',2000),audio:{...event().audio,maxMs:2000,lowerMs:2000}}).status,'fail');
 assert.equal(M.verdict({...event(),stateChanges:[{kind:'connection',state:'disconnected'}]}).status,'fail');
 assert.equal(M.verdict({...event(),descriptionChanges:[{}]}).status,'fail');
 assert.equal(M.verdict({...event(),error:'failed'}).status,'fail');
 assert.equal(M.verdict({...event(),video:{method:'presentation-only',maxMs:33,countAfter:1}}).status,'inconclusive');
});
test('hold combines invalid and pending results without false pass',()=>{
 const good={status:'pass',reasons:[]};
 assert.equal(M.combine(good,{status:'pending'}).status,'inconclusive');
 assert.equal(M.combine(good,{status:'pass',verdict:good}).status,'pass');
 assert.equal(M.combine(good,{status:'invalid',verdict:{status:'invalid',reasons:['page hidden']}}).status,'invalid');
 const h={...event('baseline'),video:{method:'decoded counter',...M.gap([0,33,66,2500,59970],0,60000)}};
 assert.equal(M.verdict(h,2000).status,'fail'); // recovery at end cannot erase hold freeze
});
test('baseline noise floor excludes invalid or unresolved windows',()=>{
 const entry=(n,status)=>({video:{maxMs:n},audio:{maxMs:n/2},windowVerdict:{status}});
 assert.deepEqual(M.noiseFloor([entry(40,'pass'),entry(60,'pass'),entry(999,'invalid')]),{video:{count:2,medianMs:50,maxMs:60},audio:{count:2,medianMs:25,maxMs:30},excluded:1});
});
test('counter resets and missing counters remain unknown',()=>{
 assert.deepEqual(M.delta({packetsLost:2,freezeCount:3},{packetsLost:1,freezeCount:4,nackCount:'unverified'}),{packetsLost:'unverified',freezeCount:1,nackCount:'unverified'});
});

test('multiple events plus continuation retain conservative burst bounds',()=>{
 const a=M.concealment([row(0),row(200,7680,2),row(250,8640,2)],0,250,48000);
 assert.equal(a.maxMs,180);assert.equal(a.lowerMs,90);
 assert.equal(M.verdict({...event(),audio:a}).status,'inconclusive');
});
test('ended audio cannot pass merely because earlier packets flowed',()=>{
 assert.equal(M.verdict({...event(),audio:{...event().audio,lastPacketAgeMs:2100}}).status,'fail');
});

test('a hidden hold invalidates even a failed event window',()=>{
 assert.deepEqual(M.combine({status:'fail',reasons:['media gap exceeds threshold']},{status:'invalid',verdict:{status:'invalid',reasons:['page hidden']}}),{status:'invalid',reasons:['page hidden']});
});

test('counter decoder accepts R-channel variation and rejects ambiguous or mismatched inverse row',()=>{
 const bytes=new Uint8ClampedArray(128);
 const n=43789;
 for(let bit=0;bit<16;bit++){const on=!!(n & (1<<bit));bytes[bit*4]=on?230:12;bytes[(bit+16)*4]=on?12:230;}
 assert.equal(M.decodeCounter(bytes),n);
 bytes[0]=120;assert.equal(M.decodeCounter(bytes),'unverified');
 bytes[0]=bytes[64];assert.equal(M.decodeCounter(bytes),'unverified');
});
test('in-flight pre-issue content is not first new content; largest gap recovery is separate',()=>{
 const frames=[{atMs:-20,counter:99,advanced:true},{atMs:14,counter:100,advanced:true},{atMs:400,counter:111,advanced:true},{atMs:433,counter:112,advanced:true}];
 const m=M.content(frames,0,450,0,110);
 assert.equal(m.firstNewContentMs,400);assert.equal(m.contentResumedMs,400);assert.equal(m.maxStartMs,14);assert.equal(m.maxMs,386);
 assert.equal(M.content(frames.slice(0,2),0,450,0,110).firstNewContentMs,'unverified');
 assert.equal(M.content(frames.slice(0,2),0,450,0,110).contentResumedMs,'unverified');
 assert.equal(M.content(frames,0,450,0).firstNewContentMs,'unverified');
});
test('under-threshold windows pass despite starved/unreadable diagnostics',()=>{
 const e={...event(),starved:true,video:{...event().video,maxStartMs:0,maxEndMs:80,counterReadErrors:1},starvedPeriods:[{startMs:0,endMs:80}],unreadPeriods:[{startMs:20,endMs:60}],longTasks:[{atMs:10,endMs:70}],samplingErrors:[{}]};
 assert.equal(M.verdict(e).status,'pass');
 const holdVerdict=M.verdict({...e,kind:'baseline'},2000);
 assert.equal(holdVerdict.status,'pass');
 assert.equal(M.combine({status:'pass',reasons:[]},{status:holdVerdict.status,verdict:holdVerdict}).status,'pass');
});
test('unreadable counter downgrades only the largest gap it overlaps',()=>{
 const e={...event('move',150),video:{...event().video,maxMs:150,maxStartMs:100,maxEndMs:250,counterReadErrors:1},unreadPeriods:[{startMs:140,endMs:200}]};
 assert.equal(M.verdict(e).status,'inconclusive');
 assert.equal(M.verdict({...e,unreadPeriods:[{startMs:300,endMs:330}]}).status,'fail');
});
test('hold downgrades only when overlapping delay could bring the failing gap under 2s',()=>{
 const h={...event('baseline'),video:{...event().video,maxMs:2050,maxStartMs:100,maxEndMs:2150},longTasks:[{atMs:200,endMs:260},{atMs:240,endMs:300}]};
 assert.equal(M.gapDiagnostics(h).overlapMs,100); // union, not double counted
 assert.equal(M.verdict(h,2000).status,'inconclusive');
 assert.equal(M.verdict({...h,video:{...h.video,maxMs:2200}},2000).status,'fail');
 assert.equal(M.verdict({...h,longTasks:[{atMs:2300,endMs:2500}]},2000).status,'fail');
 assert.equal(M.verdict({...h,audio:{...h.audio,maxMs:2200,lowerMs:2200}},2000).status,'fail');
});
test('single-draw readback snapshots both rows and reads only 16x2 pixels',()=>{
 const pixels=(n)=>{const bytes=new Uint8ClampedArray(128);for(let bit=0;bit<16;bit++){const on=!!(n & (1<<bit));bytes[bit*4]=on?230:12;bytes[(bit+16)*4]=on?12:230;}return bytes;};
 let draws=0,reads=0,frame;
 const video={videoWidth:320,videoHeight:240};
 const context={drawImage(...args){draws++;frame=pixels(draws===1?123:124);assert.deepEqual(args,[video,0,32,320,64,0,0,16,2]);},getImageData(...args){reads++;assert.deepEqual(args,[0,0,16,2]);return {data:frame};}};
 const read=M.readCounterBand(context,video);assert.equal(draws,1);assert.equal(reads,1);assert.equal(read.counter,123);assert.equal(read.rawValues.length,32);
});
test('stopped or sparse NetEq playout is inconclusive despite zero concealment',()=>{
 const a=row(0),b=row(1000);b.audio.totalSamplesReceived=40000;
 const audio=M.concealment([a,b],0,1000,48000);assert.equal(audio.maxMs,0);assert.equal(audio.playoutCoverage,40000/48000);
 assert.equal(M.verdict({...event(),audio}).status,'inconclusive');
 b.audio.totalSamplesReceived=43200;assert.equal(M.verdict({...event(),audio:M.concealment([a,b],0,1000,48000)}).status,'pass');
 b.audio.totalSamplesReceived=0;assert.equal(M.verdict({...event(),audio:M.concealment([a,b],0,1000,48000),audioDelta:{packetsReceived:0}}).status,'inconclusive');
});
test('missing silent concealment diagnostic does not veto total-counter measurement',()=>{
 const a=row(0),b=row(1000,2400,1);delete a.audio.silentConcealedSamples;delete b.audio.silentConcealedSamples;
 const audio=M.concealment([a,b],0,1000,48000);assert.equal(audio.maxMs,50);assert.equal(audio.nonSilentConcealedMs,'unverified');assert.equal(M.verdict({...event(),audio}).status,'pass');
});
const baseline=(n=40,status='pass',audio=0)=>({video:{maxMs:n},audio:{maxMs:audio},windowVerdict:{status,reasons:[]}});
const floor=(max=40,count=5,audio=0)=>M.noiseFloor(Array.from({length:count},()=>baseline(max,'pass',audio)));
test('raw passing move/drain gaps pass with a high or missing floor',()=>{
 for(const kind of ['move','drain'])for(const f of [floor(84),floor(120),floor(40,1),floor(40,0)]){
  assert.deepEqual(M.verdict({...event(kind,83.3),noiseFloor:f,noiseGate:M.noiseGate(f)}),{status:'pass',reasons:[]});
 }
});
test('would-be move/drain gap failures become inconclusive with a high floor',()=>{
 for(const kind of ['move','drain'])for(const f of [floor(80),floor(84),floor(40,5,80)]){
  assert.deepEqual(M.verdict({...event(kind,110),noiseFloor:f,noiseGate:M.noiseGate(f)}),{status:'inconclusive',reasons:['noise floor near threshold']});
  const audioVerdict=M.verdict({...event(kind),audio:{...event().audio,maxMs:110,lowerMs:110},noiseFloor:f,noiseGate:M.noiseGate(f)});
  assert.deepEqual(audioVerdict,f.audio.maxMs===0?{status:'fail',reasons:['media gap exceeds threshold']}:{status:'inconclusive',reasons:['noise floor near threshold']});
 }
});
test('raw move/drain gap failures remain fail with enough low-floor baselines',()=>{
 for(const kind of ['move','drain']){
  // 110 - 40 would pass: the floor must never be subtracted.
  assert.deepEqual(M.verdict({...event(kind,110),noiseGate:M.noiseGate(floor())}),{status:'fail',reasons:['media gap exceeds threshold']});
  assert.equal(M.verdict({...event(kind),audio:{...event().audio,maxMs:110,lowerMs:110},noiseGate:M.noiseGate(floor())}).status,'fail');
 }
});
test('would-be gap failures with too few valid baselines are inconclusive',()=>{
 for(const kind of ['move','drain'])for(const f of [floor(40,4),floor(40,0),M.noiseFloor([...Array.from({length:4},()=>baseline()),baseline(1,'invalid')])]){
  assert.deepEqual(M.verdict({...event(kind,110),noiseFloor:f,noiseGate:M.noiseGate(f)}),{status:'inconclusive',reasons:['too few valid baselines']});
 }
 assert.equal(M.verdict({...event('move',110),noiseGate:undefined}).status,'inconclusive');
});
test('baseline count and headroom are configurable; equality is near threshold',()=>{
 assert.equal(M.noiseGate(floor(79)).ready,true);
 assert.equal(M.noiseGate(floor(80)).ready,false);
 assert.equal(M.noiseGate(floor(84),5,100,0.9).ready,true);
 assert.equal(M.noiseGate(floor(79),5,100,0.7).ready,false);
 assert.equal(M.noiseGate(floor(40,2),2).ready,true);
 assert.equal(M.noiseGate(floor(40,5),6).ready,false);
 assert.equal(M.verdict({...event('move',110),noiseGate:M.noiseGate(floor(84),5,100,0.9)}).status,'fail');
 assert.equal(M.verdict({...event('move',110),noiseGate:M.noiseGate(floor(40,2),2)}).status,'fail');
});
test('kills, holds and independent failures bypass the noise gate',()=>{
 const noiseGate=M.noiseGate(floor(84,1));
 assert.equal(M.verdict({...event('kill'),noiseGate}).status,'pass');
 assert.equal(M.verdict({...event('kill',2000),noiseGate}).status,'fail');
 assert.equal(M.verdict({...event('baseline',2000),noiseGate},2000).status,'fail');
 assert.deepEqual(M.verdict({...event('move',110),noiseGate,error:'request failed'}),{status:'fail',reasons:['action failed']});
 assert.deepEqual(M.verdict({...event('move',110),noiseGate,descriptionChanges:[{}]}),{status:'fail',reasons:['ICE restart or renegotiation']});
 assert.deepEqual(M.verdict({...event('move',110),noiseGate,connectionStateAfter:'disconnected'}),{status:'fail',reasons:['connection changed']});
});
test('noise diagnostics compare median and max, and bound video jitter by one source frame',()=>{
 const noiseFloor=M.noiseFloor([baseline(66, 'pass',10),baseline(84,'pass',20)]);
 const e={...event('move',110),noiseFloor};
 assert.deepEqual(M.noiseDiagnostics(e,1000/30),{excessOverFloorMs:{video:{medianMs:35,maxMs:26},audio:{medianMs:65,maxMs:60}},frameIntervalMs:1000/30,withinBaselineJitter:true,baselineJitterAvailable:true});
 assert.equal(M.noiseDiagnostics({...e,video:{maxMs:84+1000/30}},1000/30).withinBaselineJitter,true);
 assert.equal(M.noiseDiagnostics({...e,video:{maxMs:118}},1000/30).withinBaselineJitter,false);
 assert.equal(M.noiseDiagnostics({...e,video:{maxMs:83.3}},1000/30).excessOverFloorMs.video.maxMs,0);
 assert.equal(M.noiseDiagnostics(e,M.UNKNOWN).baselineJitterAvailable,false);
 const missing=M.noiseDiagnostics({...e,noiseFloor:floor(40,0)},1000/30);
 assert.equal(missing.withinBaselineJitter,false);assert.equal(missing.baselineJitterAvailable,false);assert.equal(missing.excessOverFloorMs.video.maxMs,M.UNKNOWN);
});

test('a delayed callback at the gap start cannot shrink a true 150ms gap to 90ms',()=>{
 const frames=[{atMs:66,presentationTimeMs:66,counter:1,advanced:true},{atMs:160,presentationTimeMs:100,counter:2,advanced:true},{atMs:250,presentationTimeMs:250,counter:3,advanced:true}];
 const video={method:'decoded counter',...M.content(frames,80,270,170,2)};
 assert.equal(video.maxMs,150);assert.equal(video.maxStartMs,100);assert.equal(video.maxEndMs,250);
 assert.equal(video.firstNewContentMs,80);assert.equal(video.contentResumedMs,80);
 const e={...event('move'),video};
 assert.equal(M.verdict(e).status,'fail');
 assert.equal(M.verdict({...e,longTasks:[{atMs:100,endMs:160}]}).status,'inconclusive');
 // A callback delay below the long-task reporting cutoff also cannot shrink it.
 const shorter=frames.map((f)=>f.counter===2?{...f,atMs:140}:f);
 assert.equal(M.content(shorter,80,270,170,2).maxMs,150);
 // New pixels seen at 250 must not be reported as seen at older metadata 200.
 assert.equal(M.content(frames.map((f)=>f.counter===3?{...f,presentationTimeMs:200}:f),80,270,170,2).firstNewContentMs,80);
});
test('open content gaps begin at presentation time even if the last callback was delayed',()=>{
 const frames=[{atMs:60,presentationTimeMs:0,counter:1,advanced:true}];
 const video=M.content(frames,80,150,80,1);
 assert.equal(video.maxMs,150);assert.equal(video.maxEnded,false);assert.equal(video.contentResumedMs,M.UNKNOWN);
});
test('baseline floor recovers over the last N valid baselines and excludes freezes',()=>{
 const history=[baseline(84),...Array.from({length:5},()=>baseline(40))];
 assert.equal(M.noiseFloor(history).video.maxMs,40);assert.equal(M.noiseGate(M.noiseFloor(history)).ready,true);
 assert.equal(M.noiseFloor(history,6).video.maxMs,84);assert.equal(M.noiseFloor(history,2).video.count,2);
 const freezes=[baseline(100),baseline(100,'fail'),baseline(40,'pass',100),baseline(200,'fail')];
 const recent=M.noiseFloor([...history,...freezes]);
 assert.equal(recent.video.maxMs,40);assert.equal(recent.video.count,5);assert.equal(recent.excluded,4);
 assert.equal(M.noiseFloor([baseline(84),baseline(40),baseline(50),baseline(60)],3).video.maxMs,60);
 assert.equal(M.noiseFloor([baseline(84),...freezes,baseline(40),baseline(50),baseline(60)],3).video.maxMs,60);
});
test('event diagnostic overlap downgrades only if it could bring the gap below threshold',()=>{
 const e={...event('kill',2500),video:{...event('kill',2500).video,maxStartMs:100,maxEndMs:2600},longTasks:[{atMs:100,endMs:105}]};
 assert.deepEqual(M.verdict(e),{status:'fail',reasons:['media gap exceeds threshold']});
 assert.equal(M.verdict({...e,longTasks:[{atMs:100,endMs:600}]}).status,'fail'); // adjusted exactly 2s
 assert.equal(M.verdict({...e,longTasks:[{atMs:100,endMs:601}]}).status,'inconclusive');
 const move={...event('move',150),video:{...event('move',150).video,maxStartMs:100,maxEndMs:250},longTasks:[{atMs:100,endMs:105}]};
 assert.equal(M.verdict(move).status,'fail');
 assert.equal(M.verdict({...move,longTasks:[{atMs:100,endMs:150}]}).status,'fail');
 assert.equal(M.verdict({...move,longTasks:[{atMs:100,endMs:151}]}).status,'inconclusive');
});
test('content recovery selects the largest gap ending after issue, excluding larger lookback gaps',()=>{
 const frames=[{atMs:-1000,counter:1,advanced:true},{atMs:-200,counter:2,advanced:true},{atMs:-20,counter:3,advanced:true},{atMs:130,counter:4,advanced:true},{atMs:160,counter:5,advanced:true}];
 const measured=M.content(frames,-900,180,0,3);
 assert.equal(measured.maxMs,800);assert.equal(measured.maxEndMs,-200);
 assert.equal(measured.contentResumedMs,130);assert.equal(measured.firstNewContentMs,130);
 const open=M.content(frames.slice(0,3),-900,200,0,3);
 assert.equal(open.maxMs,800);assert.equal(open.contentResumedMs,M.UNKNOWN);
});

test('gross move/drain gaps remain failures regardless of noise-gate state',()=>{
 for(const kind of ['move','drain'])for(const noiseFloor of [floor(84),floor(40,1),floor(40,0)]){
  const e={...event(kind,400),noiseFloor,noiseGate:M.noiseGate(noiseFloor)};
  assert.equal(e.noiseGate.ready,false);
  assert.deepEqual(M.verdict(e),{status:'fail',reasons:['media gap exceeds threshold']});
  assert.deepEqual(M.verdict({...e,video:{...e.video,maxMs:80},audio:{...e.audio,maxMs:400,lowerMs:400}}),{status:'fail',reasons:['media gap exceeds threshold']});
 }
});
test('gross failure boundary is threshold plus the matching floor maximum, inclusively',()=>{
 for(const kind of ['move','drain']){
  const noiseFloor=floor(84,5,80),e={...event(kind,184),noiseFloor,noiseGate:M.noiseGate(noiseFloor)};
  assert.equal(M.verdict(e).status,'fail');
  assert.equal(M.verdict({...e,video:{...e.video,maxMs:183.999}}).status,'inconclusive');
  assert.equal(M.verdict({...e,video:{...e.video,maxMs:80},audio:{...e.audio,maxMs:180,lowerMs:180}}).status,'fail');
  assert.equal(M.verdict({...e,video:{...e.video,maxMs:80},audio:{...e.audio,maxMs:179.999,lowerMs:179.999}}).status,'inconclusive');
  // A high video floor cannot hide an audio failure against a zero audio floor.
  const silentFloor=floor(84),audioFailure={...e,noiseFloor:silentFloor,noiseGate:M.noiseGate(silentFloor),video:{...e.video,maxMs:80},audio:{...e.audio,maxMs:100,lowerMs:100}};
  assert.equal(M.verdict(audioFailure).status,'fail');
 }
});
test('without a floor the gross-failure boundary uses the recorded frame interval',()=>{
 for(const kind of ['move','drain'])for(const frameIntervalMs of [1000/30,50]){
  const noiseFloor=floor(40,0),e={...event(kind,100+frameIntervalMs),frameIntervalMs,noiseFloor,noiseGate:M.noiseGate(noiseFloor)};
  assert.equal(M.verdict(e).status,'fail');
  assert.equal(M.verdict({...e,video:{...e.video,maxMs:100+frameIntervalMs-0.001}}).status,'inconclusive');
  assert.equal(M.verdict({...e,video:{...e.video,maxMs:80},audio:{...e.audio,maxMs:100+frameIntervalMs,lowerMs:100+frameIntervalMs}}).status,'fail');
 }
 const e={...event('move',400),noiseFloor:floor(40,0),noiseGate:{ready:false},frameIntervalMs:M.UNKNOWN};
 assert.equal(M.verdict(e).status,'fail'); // no measured noise allowance
});
test('gross audio failure requires a proven burst, not just a sampled upper bound',()=>{
 const noiseFloor=floor(84,5,80),e={...event('move'),noiseFloor,noiseGate:M.noiseGate(noiseFloor),audio:{...event().audio,maxMs:400,lowerMs:120,exact:false}};
 assert.equal(M.verdict(e).status,'inconclusive');
 assert.equal(M.verdict({...e,audio:{...e.audio,lowerMs:180}}).status,'fail');
 assert.equal(M.verdict({...e,audio:{...e.audio,lowerMs:80}}).status,'inconclusive');
});

test('first live frame is null for moves, drains, and baselines even with live media',()=>{
 const frames=[{atMs:60,counter:10,advanced:true,sourceCounterAtObservation:11},
  {atMs:85,counter:12,advanced:true}];
 for(const kind of ['move','drain','baseline']) {
  assert.equal(M.firstLiveFrame(kind,frames,0,1400,60),null);
  assert.equal(M.firstLiveFrame(kind,[],0,1400,M.UNKNOWN),null);
 }
 assert.equal(M.firstLiveFrame('kill',frames,0,1400,60),85);
});
test('first live frame waits past the source watermark at recovery, not advancing replay',()=>{
 const frames=[
  {atMs:0,counter:100,advanced:true,sourceCounterAtObservation:102},
  {atMs:400,counter:101,advanced:true,sourceCounterAtObservation:114},
  {atMs:433,counter:110,advanced:true,sourceCounterAtObservation:115},
  {atMs:466,counter:114,advanced:true,sourceCounterAtObservation:116},
  {atMs:499,counter:115,advanced:true,sourceCounterAtObservation:117}
 ];
 const m=M.content(frames,10,520,10,102);
 assert.equal(m.contentResumedMs,390);
 assert.equal(m.firstNewContentMs,423);
 assert.equal(M.firstLiveFrame('kill',frames,10,520,m.contentResumedMs),489);
 assert.equal(M.firstLiveFrame('kill',frames,10,480,m.contentResumedMs),M.UNKNOWN);
 assert.equal(M.firstLiveFrame('kill',frames,10,520,M.UNKNOWN),M.UNKNOWN);
});
test('missing recovery watermark and stale or unreadable frames cannot prove live video',()=>{
 const frames=[{atMs:400,counter:10,advanced:true,sourceCounterAtObservation:20},
  {atMs:420,counter:M.UNKNOWN,advanced:false},
  {atMs:450,counter:21,advanced:false},
  {atMs:470,counter:20,advanced:true}];
 assert.equal(M.firstLiveFrame('kill',frames,0,500,400),M.UNKNOWN);
 assert.equal(M.firstLiveFrame('kill',[{atMs:400,counter:10,advanced:true},...frames.slice(1)],0,500,400),M.UNKNOWN);
 assert.equal(M.firstLiveFrame('kill',frames,0,500,300),M.UNKNOWN);
});
test('live frame comparison uses unwrapped counters across the 16-bit boundary',()=>{
 const frames=[{atMs:400,counter:65534,advanced:true,sourceCounterAtObservation:65536},
  {atMs:433,counter:65536,advanced:true}, {atMs:466,counter:65537,advanced:true}];
 assert.equal(M.firstLiveFrame('kill',frames,0,500,400),466);
});
test('shared counter painter survives the unchanged reader at pattern and 720p geometry',()=>{
 for(const [width,height] of [[640,480],[1280,720]]) {
  for(const n of [1,43789,65535,65536]) {
   const rectangles=[];
   const painter={fillStyle:null,fillRect(x,y,w,h){rectangles.push({x:x*width/640,y:y*height/480,w:w*width/640,h:h*height/480,color:this.fillStyle});}};
   M.drawCounterBand(painter,n);
   const bytes=new Uint8ClampedArray(128);
   const reader={drawImage(video,x,y,w,h,dx,dy,dw,dh){
    assert.equal(y,height*64/480);assert.equal(h,height*128/480);
    for(let row=0;row<2;row++)for(let bit=0;bit<16;bit++) {
     const sx=(bit+0.5)*width/16,sy=height*(96+row*64)/480;
     const rect=rectangles.find((r)=>sx>=r.x && sx<r.x+r.w && sy>=r.y && sy<r.y+r.h);
     assert.ok(rect);bytes[(row*16+bit)*4]=rect.color==='#fff'?255:0;
    }
   },getImageData(){return {data:bytes};}};
   assert.equal(M.readCounterBand(reader,{videoWidth:width,videoHeight:height}).counter,n & 65535);
  }
 }
});
