'use strict';
((root) => {
  const UNKNOWN = 'unverified';
  function gap(times,start,end) {
    const before=times.filter((at)=>at<=start).at(-1);
    if(before===undefined)return {maxMs:UNKNOWN,maxStartMs:UNKNOWN,maxEndMs:UNKNOWN,maxEnded:false,firstAfterMs:UNKNOWN,countAfter:0};
    const after=times.filter((at)=>at>start && at<=end),points=[before,...after];
    let maxMs=end-points.at(-1),maxStartMs=points.at(-1),maxEndMs=end,maxEnded=false;
    for(let i=1;i<points.length;i++)if(points[i]-points[i-1]>maxMs){maxMs=points[i]-points[i-1];maxStartMs=points[i-1];maxEndMs=points[i];maxEnded=true;}
    return {maxMs,maxStartMs,maxEndMs,maxEnded,firstAfterMs:after.length?after[0]-start:UNKNOWN,countAfter:after.length};
  }
  function delta(before,after) {
    return Object.fromEntries(Object.keys(after).map((key)=>[key,typeof before[key]==='number' && typeof after[key]==='number' && after[key]>=before[key]?after[key]-before[key]:UNKNOWN]));
  }
  function windowStart(issued,previousEnd=-Infinity) { return Math.max(issued-1000,previousEnd); }
  function content(frames,start,end,issued=start,sourceCounterAtIssue=UNKNOWN) {
    const advances=frames.filter((f)=>f.advanced && f.atMs<=end);
    const first=typeof sourceCounterAtIssue==='number' && advances.find((f)=>f.atMs>issued && f.counter>sourceCounterAtIssue);
    const before=advances.findLastIndex((f)=>f.atMs<=start),intervals=[];
    // A newer canvas snapshot is observed at callback time. Its gap begins at
    // the previous advancing frame's presentation time, so delayed callbacks
    // can enlarge an interval but cannot shorten it.
    const presented=(f)=>f.presentationTimeMs ?? f.atMs;
    if(before>=0){
      for(let i=before+1;i<advances.length;i++)intervals.push({maxMs:advances[i].atMs-presented(advances[i-1]),maxStartMs:presented(advances[i-1]),maxEndMs:advances[i].atMs,maxEnded:true});
      intervals.push({maxMs:end-presented(advances.at(-1)),maxStartMs:presented(advances.at(-1)),maxEndMs:end,maxEnded:false});
    }
    // Prefer the open tail on ties: recovery must be demonstrated.
    const largest=(items)=>items.reduceRight((best,item)=>!best || item.maxMs>best.maxMs?item:best,null);
    const measured=largest(intervals),recovery=largest(intervals.filter((i)=>i.maxEndMs>issued));
    const after=advances.filter((f)=>f.atMs>start);
    return {...(measured || {maxMs:UNKNOWN,maxStartMs:UNKNOWN,maxEndMs:UNKNOWN,maxEnded:false}),
      firstAfterMs:after.length?after[0].atMs-start:UNKNOWN,countAfter:after.length,
      firstNewContentMs:first?first.atMs-issued:UNKNOWN,
      contentResumedMs:recovery?.maxEnded?recovery.maxEndMs-issued:UNKNOWN,
      replayedOrOld:frames.some((f)=>f.atMs>=start && f.atMs<=end && f.old),
      counterReadErrors:frames.filter((f)=>f.atMs>=start && f.atMs<=end && f.counter===UNKNOWN).length};
  }
  // Takeovers only: planned moves and drains have no kill-to-live interval.
  // Wait beyond the source watermark at content recovery. An advancing
  // replay frame can end a gap while still carrying content from the outage.
  function firstLiveFrame(kind,frames,issued,end,contentResumedMs) {
    if(kind!=='kill')return null;
    if(typeof contentResumedMs!=='number' || contentResumedMs<0)return UNKNOWN;
    const resumed=frames.find((f)=>f.advanced && f.atMs-issued===contentResumedMs);
    if(!resumed || typeof resumed.sourceCounterAtObservation!=='number')return UNKNOWN;
    const first=frames.find((f)=>f.advanced && f.atMs>resumed.atMs && f.atMs<=end && f.counter>resumed.sourceCounterAtObservation);
    return first?first.atMs-issued:UNKNOWN;
  }
  function drawCounterBand(context,n) {
    for(let bit=0;bit<16;bit++) {
      const on=!!(n & (1<<bit));
      context.fillStyle=on?'#fff':'#000';context.fillRect(bit*40,64,40,64);
      context.fillStyle=on?'#000':'#fff';context.fillRect(bit*40,128,40,64);
    }
  }
  // One compositor snapshot for both counter rows; never read a full frame.
  function readCounterBand(context,video) {
    const width=video.videoWidth,height=video.videoHeight;
    if(!width || !height)throw new Error('video dimensions unavailable');
    context.drawImage(video,0,height*64/480,width,height*128/480,0,0,16,2);
    const bytes=context.getImageData(0,0,16,2).data;
    return {counter:decodeCounter(bytes),rawValues:Array.from({length:32},(_,i)=>bytes[i*4])};
  }
  function decodeCounter(bytes) {
    if(bytes.length!==128)return UNKNOWN;
    let counter=0;
    const bit=(v)=>v<=80?0:v>=175?1:null;
    for(let i=0;i<16;i++) {
      const a=bit(bytes[i*4]),b=bit(bytes[(i+16)*4]);
      if(a===null || b===null || a===b)return UNKNOWN;
      if(a)counter|=1<<i;
    }
    return counter;
  }
  function counterObservation(counter,previousMax) {
    return {counter,advanced:typeof counter==='number' && (previousMax===null || counter>previousMax),old:typeof counter==='number' && previousMax!==null && counter<=previousMax};
  }
  function starved(previousPresented,presented) { return typeof previousPresented==='number' && presented-previousPresented>1; }
  function hiddenDuring(periods,start,end) { return periods.some((p)=>p.startMs<=end && (p.endMs??Infinity)>=start); }
  // Counters describe playout, not the time getStats() returns. Associate the
  // total concealed samples with concealment events. Multiple events between stats
  // yield bounds, never an invented exact longest burst.
  function concealment(samples,start,end,rate) {
    const rows=samples.filter((s)=>s.atMs<=end), first=rows.findLastIndex((s)=>s.atMs<=start);
    const unknown={method:'sampling-only',maxMs:UNKNOWN,lowerMs:UNKNOWN,exact:false,concealmentEvents:UNKNOWN,totalSamplesReceived:UNKNOWN,lastPacketReceivedTimestampDeltas:[]};
    if(first<0 || !(rate>0)) return unknown;
    let sum=0,nonSilentSum=0,max=0,lower=0,events=0,total=0,burst=0,lastEvent=null,groupSize=1,open=false,exact=true;
    const packetDeltas=[];
    for(let i=first+1;i<rows.length;i++) {
      const a=rows[i-1].audio,b=rows[i].audio,d=delta(a,b);
      if(['concealedSamples','concealmentEvents','totalSamplesReceived'].some((k)=>typeof d[k]!=='number')) return unknown;
      const nonSilent=typeof d.silentConcealedSamples==='number' && d.silentConcealedSamples<=d.concealedSamples?d.concealedSamples-d.silentConcealedSamples:UNKNOWN;
      sum+=d.concealedSamples;nonSilentSum=typeof nonSilentSum==='number' && typeof nonSilent==='number'?nonSilentSum+nonSilent:UNKNOWN;events+=d.concealmentEvents;total+=d.totalSamplesReceived;
      if(typeof a.lastPacketReceivedTimestamp==='number' && typeof b.lastPacketReceivedTimestamp==='number') packetDeltas.push(b.lastPacketReceivedTimestamp-a.lastPacketReceivedTimestamp);
      if(d.concealedSamples>0) {
        if(d.concealmentEvents>0) {
          groupSize=d.concealmentEvents+(open?groupSize:0);
          burst=(open?burst:0)+d.concealedSamples;
          if(groupSize>1)exact=false;
        }else if(lastEvent===null || lastEvent!==b.concealmentEvents || !open) {burst=d.concealedSamples;groupSize=1;}
        else burst+=d.concealedSamples;
        lower=Math.max(lower,burst/groupSize);
        max=Math.max(max,burst);lastEvent=b.concealmentEvents;open=true;
      }else open=false;
    }
    const elapsedMs=rows.at(-1).atMs-rows[first].atMs,expectedSamples=elapsedMs*rate/1000;
    return {method:'total concealment samples',maxMs:max/rate*1000,lowerMs:lower/rate*1000,exact,
      concealedSamples:sum,nonSilentConcealedSamples:nonSilentSum,nonSilentConcealedMs:typeof nonSilentSum==='number'?nonSilentSum/rate*1000:UNKNOWN,
      playoutCoverage:expectedSamples>0?total/expectedSamples:UNKNOWN,playoutCoverageMinimum:0.9,expectedSamples,elapsedMs,concealmentEvents:events,totalSamplesReceived:total,measurementStartMs:rows[first].atMs,measurementEndMs:rows.at(-1).atMs,lastPacketReceivedTimestampDeltas:packetDeltas};
  }
  function overlappingMs(periods,start,end) {
    if(typeof start!=='number' || typeof end!=='number')return 0;
    const clips=periods.map((p)=>[Math.max(start,p.startMs??p.atMs),Math.min(end,p.endMs)]).filter(([a,b])=>b>a).sort((a,b)=>a[0]-b[0]);
    let total=0,last=start;
    for(const [a,b] of clips){total+=Math.max(0,b-Math.max(a,last));last=Math.max(last,b);}
    return total;
  }
  // Calibrate from observed draws before the window, so a stall within the
  // window cannot inflate its own expected cadence. Use up to 60 intervals.
  function sourceDiagnostics(times,start,end) {
    const draws=times.filter((at)=>at<=end);
    const intervals=draws.slice(1).map((at,i)=>({startMs:draws[i],endMs:at,ms:at-draws[i]})).filter((p)=>p.ms>0);
    const before=intervals.filter((p)=>p.endMs<=start);
    const values=(before.length?before:intervals).slice(-60).map((p)=>p.ms).sort((a,b)=>a-b);
    const cadence=values.length?(values[Math.floor((values.length-1)/2)]+values[Math.floor(values.length/2)])/2:UNKNOWN;
    const periods=typeof cadence==='number'?intervals.filter((p)=>p.ms>cadence*1.5 && p.endMs>=start):[];
    const last=draws.at(-1);
    if(typeof cadence==='number' && last!==undefined && end-last>cadence*1.5)periods.push({startMs:last,endMs:end,ms:end-last,open:true});
    return {frameIntervalMs:cadence,sourceStarvedPeriods:periods};
  }
  function gapDiagnostics(e) {
    const periods=[...(e.starvedPeriods || []),...(e.sourceStarvedPeriods || []),...(e.unreadPeriods || []),...(e.longTasks || [])];
    const overlapMs=overlappingMs(periods,e.video.maxStartMs,e.video.maxEndMs);
    return {overlapMs,adjustedVideoGapMs:typeof e.video.maxMs==='number'?Math.max(0,e.video.maxMs-overlapMs):UNKNOWN};
  }
  function noiseGate(floor,requiredBaselines=5,threshold=100,headroomRatio=0.8) {
    const enoughBaselines=['video','audio'].every((kind)=>floor?.[kind]?.count>=requiredBaselines && typeof floor[kind].maxMs==='number');
    const nearThreshold=enoughBaselines && ['video','audio'].some((kind)=>floor[kind].maxMs>=threshold*headroomRatio);
    return {requiredBaselines,headroomRatio,thresholdMs:threshold,enoughBaselines,ready:enoughBaselines && !nearThreshold,
      reason:!enoughBaselines?'too few valid baselines':nearThreshold?'noise floor near threshold':null};
  }
  function noiseDiagnostics(e,frameIntervalMs) {
    const excessOverFloorMs=Object.fromEntries(['video','audio'].map((kind)=>[kind,Object.fromEntries(['medianMs','maxMs'].map((stat)=>[stat,
      typeof e[kind].maxMs==='number' && typeof e.noiseFloor?.[kind]?.[stat]==='number'?Math.max(0,e[kind].maxMs-e.noiseFloor[kind][stat]):UNKNOWN]))]));
    const available=typeof e.video.maxMs==='number' && typeof e.noiseFloor?.video?.maxMs==='number' && typeof frameIntervalMs==='number' && frameIntervalMs>0;
    return {excessOverFloorMs,frameIntervalMs,withinBaselineJitter:available && e.video.maxMs<=e.noiseFloor.video.maxMs+frameIntervalMs,baselineJitterAvailable:available};
  }
  function verdict(e,threshold=e.kind==='kill'?2000:100) {
    if(e.hidden)return {status:'invalid',reasons:['page hidden']};
    const uncertain=[],failures=[];
    const mediaFailure=(kind,provenGapMs)=>{
      const floorMax=e.noiseFloor?.[kind]?.maxMs;
      // Missing calibration must not hide a gross failure. Without a measured
      // floor, allow one recorded source frame interval. With neither available,
      // there is no measured noise allowance for downgrading a raw failure.
      const noiseAllowance=typeof floorMax==='number'?floorMax:typeof e.frameIntervalMs==='number' && e.frameIntervalMs>0?e.frameIntervalMs:0;
      if(['move','drain'].includes(e.kind) && !e.noiseGate?.ready && provenGapMs<threshold+noiseAllowance)uncertain.push(e.noiseGate?.reason || 'too few valid baselines');
      else failures.push('media gap exceeds threshold');
    };
    if(e.error || (e.kind!=='baseline' && (!e.to || e.to===e.from)))failures.push('action failed');
    if(e.video.method==='presentation-only')uncertain.push('camera: presentation-only');
    if(typeof e.video.maxMs!=='number')uncertain.push('video gap unavailable');
    else if(e.video.maxMs>=threshold) {
      const diagnostic=gapDiagnostics(e);
      if(diagnostic.overlapMs>0 && diagnostic.adjustedVideoGapMs<threshold)uncertain.push('largest video gap overlaps starvation or unreadable counter');
      else mediaFailure('video',e.video.maxMs);
    }
    if(e.audio.method==='sampling-only' || typeof e.audio.maxMs!=='number')uncertain.push('playout audio counters unavailable');
    else if(e.audio.maxMs>=threshold) {
      if(e.audio.lowerMs>=threshold)mediaFailure('audio',e.audio.lowerMs);
      else uncertain.push('multiple audio concealment events between stats');
    }
    if(typeof e.audio.playoutCoverage!=='number' || e.audio.playoutCoverage<0.9)uncertain.push('audio playout coverage below 90% or unavailable');
    if(typeof e.audio.lastPacketAgeMs==='number' && e.audio.lastPacketAgeMs>=2000)failures.push('audio packets stopped');
    if(typeof e.video.maxMs==='number' && e.video.maxMs<threshold && !(e.video.countAfter>0 && e.videoDelta.framesDecoded>0))failures.push('video did not continue');
    if(e.audio.playoutCoverage>=0.9 && !(e.audioDelta.packetsReceived>0))failures.push('audio did not continue');
    if(e.connectionStateBefore!=='connected' || e.connectionStateAfter!=='connected' || e.stateChanges.some((x)=>x.kind==='connection' && x.state!=='connected'))failures.push('connection changed');
    if(e.iceRestarts || e.renegotiations || e.descriptionChanges.length || e.stateChanges.some((x)=>x.kind==='signaling'))failures.push('ICE restart or renegotiation');
    return {status:failures.length?'fail':uncertain.length?'inconclusive':'pass',reasons:[...new Set(failures.length?failures:uncertain)]};
  }
  function combine(windowVerdict,hold) {
    if(hold.status==='invalid')return hold.verdict;
    if(windowVerdict.status!=='pass') return windowVerdict;
    if(hold.status==='pending' || hold.status==='superseded' || hold.status==='not-run') return {status:'inconclusive',reasons:['60 s hold '+hold.status]};
    return hold.verdict;
  }
  function validBaseline(e,threshold) {return !(e.sourceStarvedPeriods?.length) && e.windowVerdict.status==='pass' && ['video','audio'].every((kind)=>typeof e[kind].maxMs==='number' && e[kind].maxMs<threshold);}
  function noiseFloor(entries,baselineCount=5,threshold=100) {
    const valid=entries.filter((e)=>validBaseline(e,threshold)),recent=valid.slice(-baselineCount);
    const measure=(kind)=>{const values=recent.map((e)=>e[kind].maxMs).sort((a,b)=>a-b);return {count:values.length,medianMs:values.length?(values[Math.floor((values.length-1)/2)]+values[Math.floor(values.length/2)])/2:UNKNOWN,maxMs:values.length?values.at(-1):UNKNOWN};};
    return {video:measure('video'),audio:measure('audio'),excluded:entries.length-valid.length};
  }
  const api={UNKNOWN,gap,delta,windowStart,content,firstLiveFrame,drawCounterBand,readCounterBand,decodeCounter,counterObservation,starved,hiddenDuring,concealment,overlappingMs,sourceDiagnostics,gapDiagnostics,noiseGate,noiseDiagnostics,verdict,combine,noiseFloor};
  if(typeof module!=='undefined')module.exports=api;else root.relaisMetrics=api;
})(globalThis);
