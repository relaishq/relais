'use strict';
(() => {
  const M = window.relaisMetrics;
  const el = Object.fromEntries(['start','stop','move','drain','kill','audio','local','remote','summary','gap','moves','transitions','results','error','source','save','check'].map((id) => [id,document.getElementById(id)]));
  const params = new URLSearchParams(location.search);
  const launchToken = new URLSearchParams(location.hash.slice(1)).get('token') || '';
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve,ms));
  let call = null, lastResult = null, starting = null, lastResultsRender = -Infinity;
  const stamp = () => ({ at: new Date().toISOString(), atMs: performance.now() });
  const active = (s) => call === s && !s.stopped;
  function note(s, kind, state) { s.transitions.push({...stamp(),kind,state}); }
  function error(err) { el.error.textContent = err.message || String(err); }
  function request(path,options={}) {
    const headers=new Headers(options.headers);
    if((options.method || 'GET').toUpperCase()!=='GET' && launchToken)headers.set('X-Relais-Demo-Token',launchToken);
    return fetch(path,{...options,headers});
  }
  async function json(path, options = {}) {
    const response = await request(path, {...options,cache:'no-store'});
    const text = await response.text();
    let data; try { data = JSON.parse(text); } catch { data = {error:text}; }
    if (!response.ok) throw Object.assign(new Error(data.error || text || `HTTP ${response.status}`), {server:data});
    return data;
  }
  function stampCounter(g,canvas,n) {
    // Preserve the test pattern's normalized 640x480 geometry at 720p.
    g.save();g.scale(canvas.width/640,canvas.height/480);
    g.font='28px monospace';g.fillStyle='#fff';
    g.fillText(`${new Date().toISOString().slice(11,23)} #${n}`,24,450);
    M.drawCounterBand(g,n);
    g.restore();
  }
  async function sourceStream(s) {
    const canvas=document.createElement('canvas');
    canvas.width=s.source==='camera'?1280:640;canvas.height=s.source==='camera'?720:480;
    const g=canvas.getContext('2d');let n=0;
    const draw=()=>{
      if(!active(s))return;
      if(s.source==='camera')g.drawImage(s.cameraVideo,0,0,canvas.width,canvas.height);
      else {
        const t=performance.now()/1000;
        g.fillStyle='#16445c';g.fillRect(0,0,640,480);
        g.fillStyle='#fff';g.fillRect(260+180*Math.sin(t),140+100*Math.cos(t),100,100);
        g.font='28px monospace';g.fillText('Relais cluster echo',24,48);
      }
      s.sourceCounter=++n;stampCounter(g,canvas,n);s.sourceFrames.push(performance.now());
    };
    if(s.source==='camera') {
      const camera=await navigator.mediaDevices.getUserMedia({audio:false,video:{width:{ideal:1280},height:{ideal:720},frameRate:{ideal:30,max:30}}});
      if(!active(s)){camera.getTracks().forEach((t)=>t.stop());throw new Error('call ended');}
      s.cameraStream=camera;s.cameraSettings=camera.getVideoTracks()[0].getSettings();
      s.cameraVideo=document.createElement('video');s.cameraVideo.muted=true;s.cameraVideo.playsInline=true;s.cameraVideo.srcObject=camera;
      await s.cameraVideo.play();
      if(!active(s))throw new Error('call ended');
      const frame=()=>{if(!active(s))return;draw();s.cameraCallback=s.cameraVideo.requestVideoFrameCallback(frame);};
      // Advance only when the camera provides a frame. A frozen camera must
      // not masquerade as fresh content behind a timer-driven counter.
      s.cameraCallback=s.cameraVideo.requestVideoFrameCallback(frame);
    }else {draw();s.sourceTimer=setInterval(draw,1000/30);}
    s.canvasStream=canvas.captureStream(30);
    s.audioContext=new AudioContext();
    const osc=s.audioContext.createOscillator(),gain=s.audioContext.createGain(),dest=s.audioContext.createMediaStreamDestination();
    osc.frequency.value=440;gain.gain.value=0.15;osc.connect(gain).connect(dest);osc.start();
    s.audioContext.resume().catch(error);
    return new MediaStream([...dest.stream.getAudioTracks(),...s.canvasStream.getVideoTracks()]);
  }
  function credentials(sdp) { return (sdp || '').split(/\r?\n/).filter((line) => /^a=ice-(ufrag|pwd):/.test(line)).join('\n'); }
  function descriptions(s) {
    if (!active(s)) return;
    const value = {local:s.pc.localDescription?.sdp || '',remote:s.pc.remoteDescription?.sdp || ''};
    if (s.armed && s.descriptions) {
      for (const side of ['local','remote']) if (value[side] !== s.descriptions[side]) {
        const restarted = credentials(value[side]) !== credentials(s.descriptions[side]);
        s.descriptionChanges.push({...stamp(),side,iceRestart:restarted});
      }
    }
    s.descriptions = value;
  }
  const rtcTime=(at)=>at>performance.timeOrigin/2?at-performance.timeOrigin:at;
  async function readSample(s) {
    const report = await s.pc.getStats(); if (!active(s)) return;
    const all = [...report.values()];
    const counters = ['packetsReceived','packetsLost','framesDecoded','freezeCount','totalFreezesDuration','pliCount','nackCount','concealedSamples','silentConcealedSamples','concealmentEvents','totalSamplesReceived','lastPacketReceivedTimestamp'];
    const read = (kind) => {
      const stat=all.find((x)=>x.type==='inbound-rtp' && (x.kind || x.mediaType)===kind);
      const values=Object.fromEntries(counters.map((key)=>[key,typeof stat?.[key]==='number'?stat[key]:M.UNKNOWN]));
      if(kind==='audio' && stat) {
        const codec=report.get(stat.codecId);
        s.sampleRate=s.remoteAudioTrack?.getSettings().sampleRate || codec?.clockRate || null;
        if(typeof values.lastPacketReceivedTimestamp==='number' && values.packetsReceived>0) s.lastAudio=rtcTime(values.lastPacketReceivedTimestamp);
      }
      return {values,atMs:stat?rtcTime(stat.timestamp):null};
    };
    const audio=read('audio'), video=read('video'),at=audio.atMs ?? video.atMs;
    if(at===null) return;
    if(s.latest && at<=s.latest.atMs) return; // Chrome can return cached stats.
    s.latest={atMs:at,audio:audio.values,video:video.values,videoStatsAtMs:video.atMs,connectionState:s.pc.connectionState,iceConnectionState:s.pc.iceConnectionState};
    s.samples.push(s.latest);
    // Holds retain their complete counter history; prune after they finish.
    const oldest=s.hold?.status==='pending'?Math.min(at-20000,s.hold.startedMs-1000):at-20000;
    for(const [list,key] of [[s.samples,'atMs'],[s.contentFrames,'atMs'],[s.frameMeta,'atMs']]) while(list.length>1 && list[1][key]<oldest) list.shift();
    while(s.frames.length>1 && s.frames[1]<oldest) s.frames.shift();
    if(s.hold?.status==='pending') updateHold(s);
    descriptions(s);
    if (s.hold && s.hold.status === 'pending' && at >= s.hold.dueMs) finishHold(s);
  }
  function sample(s) {
    if (!s.samplePromise) s.samplePromise = readSample(s).finally(() => { s.samplePromise = null; });
    return s.samplePromise;
  }
  async function samples(s) {
    while (active(s)) {
      const at = performance.now();
      try { await sample(s); } catch (err) { if (active(s)) { s.sampleErrors.push({...stamp(),error:String(err)}); error(err); } }
      await sleep(Math.max(0,50-(performance.now()-at)));
    }
  }
  async function statuses(s) {
    while (active(s)) {
      try {
        const status = await json('/demo/status'); if (!active(s)) return;
        s.status = status; s.statusError = null;
        const owner = status.calls.find((c) => c.id === s.id)?.owner || null;
        if (s.id && owner !== s.owner) { s.owners.push({...stamp(),from:s.owner,to:owner}); s.owner = owner; }
      } catch (err) { if (active(s)) { s.statusError = String(err); s.owner = null; } }
      await sleep(250);
    }
  }
  function snapshot(s) {
    if(!s)return lastResult || {version:4,state:'idle',running:false,events:[],baselines:[]};
    let hold=s.hold || {status:'not-run',reason:'no event yet'};
    if(s.hold && M.hiddenDuring(s.hiddenPeriods,s.hold.startedMs,Math.min(s.stoppedAtMs || performance.now(),s.hold.dueMs)))hold={...hold,status:'invalid',verdict:{status:'invalid',reasons:['page hidden']}};
    const entries=s.events.map((entry)=>{const verdict=M.combine(entry.windowVerdict,hold);return {...entry,verdict,pass:verdict.status==='pass'?true:verdict.status==='fail'?false:null};});
    const connectedThroughout=!!s.readyAtMs && s.transitions.every((e)=>e.atMs<s.readyAtMs || e.kind!=='connection' || e.state==='connected' || (s.stopped && e.state==='closed' && e.atMs>=s.stoppedAtMs));
    const noRenegotiation=!s.negotiations.length && !s.descriptionChanges.length && !s.restartCalls.length;
    const statuses=entries.map((e)=>e.verdict.status);
    const status=statuses.includes('invalid')?'invalid':statuses.includes('fail')?'fail':!statuses.length || statuses.includes('inconclusive')?'inconclusive':'pass';
    return {version:4,source:s.source,id:s.id,startedAt:s.startedAt,state:s.stopped?'ended':s.pc.connectionState,running:!!s.script || !!s.busy || !!s.checklist,
      iceConnectionState:s.pc.iceConnectionState,owner:s.owner,workerPIDs:s.status?.worker_pids || {},poolSize:s.status?.pool_size,expectedPoolSize:s.status?.expected_pool_size,registrationError:s.status?.registration_error || null,
      thresholds:{moveGapMs:100,drainGapMs:100,killGapMs:2000,longHoldMs:60000},
      measurement:{video:'decoded counter: observation time minus previous presentation time',audio:'total concealedSamples at receiver sample rate',sampleRate:s.sampleRate,statsClock:'RTCStats.timestamp',settleAfterResponseMs:2000,sourceCadence:'median of up to 60 observed draw intervals before the measurement window; source gaps over 1.5 times cadence join starvation diagnostics and are excluded from baselines',timeToFirstLiveFrameMs:'kill events only; null for moves, drains, and baselines. Browser kill-request issuance to first decoded counter beyond the source watermark sampled at largest content-gap recovery; conservative upper bound including request transit and owner lookup, without subtracting server clocks. Missing kill evidence is unverified'},
      media:{videoGapMs:s.lastContent===null?M.UNKNOWN:(s.stoppedAtMs || performance.now())-s.lastContent,presentationGapMs:s.lastVideo===null?M.UNKNOWN:(s.stoppedAtMs || performance.now())-s.lastVideo,audioPacketGapMs:s.lastAudio===null?M.UNKNOWN:(s.stoppedAtMs || performance.now())-s.lastAudio},
      overall:s.latest,connectedThroughout,noRenegotiation,transitions:s.transitions,ownerChanges:s.owners,descriptionChanges:s.descriptionChanges,
      negotiationNeeded:s.negotiations,iceRestartCalls:s.restartCalls,sampleErrors:s.sampleErrors,testAudioState:s.audioContext?.state || null,audioPlayback:s.playback || 'pending',statusError:s.statusError,
      cameraSettings:s.cameraSettings || null,configuration:{baselineCount:s.baselineCount,headroomRatio:s.headroomRatio,playoutCoverageMinimum:0.9},sourceCounter:s.sourceCounter ?? M.UNKNOWN,counterReadFailures:s.counterReadFailures,
      visibility:s.hiddenPeriods,longTasks:s.longTasks,longHold:hold,events:entries,baselines:s.baselines,noiseFloor:M.noiseFloor(s.baselines,s.baselineCount),noiseGate:M.noiseGate(M.noiseFloor(s.baselines,s.baselineCount),s.baselineCount,100,s.headroomRatio),
      verdict:{status,reasons:entries.flatMap((e)=>e.verdict.reasons)},pass:status==='pass'?true:status==='fail'?false:null,srtpDecryptionFailures:M.UNKNOWN};
  }
  function render() {
    const r=snapshot(call),s=call;
    el.start.disabled=!!s && !s.stopped;el.source.disabled=!!s && !s.stopped;el.stop.disabled=!s || s.stopped;el.save.disabled=!s && !lastResult;el.check.disabled=!s || !s.armed || s.busy || !!s.script || !!s.checklist;
    for(const id of ['move','drain','kill'])el[id].disabled=!s || !s.armed || s.pc.connectionState!=='connected' || s.busy || !!s.script || !!s.checklist;
    const fmt=(ms)=>typeof ms==='number'?`${Math.round(ms)} ms`:'unverified';
    const pool=r.poolSize===undefined?'unknown':`${r.poolSize}/${r.expectedPoolSize}${r.poolSize<r.expectedPoolSize?' WARNING: pool short':''}`;
    const rows=[['Connection',r.state],['ICE',r.iceConnectionState || '—'],['Owner',r.owner || '—'],['Worker PIDs',JSON.stringify(r.workerPIDs || {})],['Pool',pool],['60 s hold',r.longHold?.status || 'not-run'],['Test audio',r.testAudioState || 'pending'],['Echo audio',r.audioPlayback || 'pending'],['Status',r.registrationError || r.statusError || 'OK'],['Baseline video median / max',`${fmt(r.noiseFloor?.video.medianMs)} / ${fmt(r.noiseFloor?.video.maxMs)}`],['Baseline audio median / max',`${fmt(r.noiseFloor?.audio.medianMs)} / ${fmt(r.noiseFloor?.audio.maxMs)}`],['Planned-event gap failure authority',r.noiseGate?.ready?'ready':r.noiseGate?.reason || 'not measured']];
    el.summary.replaceChildren(...rows.flatMap(([k,v])=>{const dt=document.createElement('dt'),dd=document.createElement('dd');dt.textContent=k;dd.textContent=v;return [dt,dd];}));
    el.gap.textContent=`Content gap ${fmt(r.media?.videoGapMs)} · packet age (diagnostic) ${fmt(r.media?.audioPacketGapMs)}`;
    el.moves.replaceChildren(...r.events.map((entry)=>{const row=document.createElement('tr');for(const value of [entry.at.slice(11,23),entry.kind,`${entry.from || '?'} → ${entry.to || '?'}`,fmt(entry.video.maxMs),fmt(entry.audio.maxMs),`${entry.verdict.status.toUpperCase()}: ${entry.verdict.reasons.join(', ')}`]){const td=document.createElement('td');td.textContent=value;row.append(td);}return row;}));
    el.transitions.textContent=JSON.stringify(r.transitions || [],null,2);
    // Rendering large JSON trees competes with media callbacks: at most 1 Hz.
    if(performance.now()-lastResultsRender>=1000){el.results.textContent=JSON.stringify(r,null,2);lastResultsRender=performance.now();}
  }
  function visibility(s) {
    const hidden=document.hidden || s.remoteCovered;
    if(hidden && !s.hiddenPeriods.at(-1)?.open)s.hiddenPeriods.push({startMs:performance.now(),open:true});
    else if(!hidden && s.hiddenPeriods.at(-1)?.open){s.hiddenPeriods.at(-1).endMs=performance.now();s.hiddenPeriods.at(-1).open=false;}
  }
  function observePage(s) {
    s.visibilityListener=()=>visibility(s);document.addEventListener('visibilitychange',s.visibilityListener);visibility(s);
    s.intersection=new IntersectionObserver((entries)=>{s.remoteCovered=!entries[0].isIntersecting || (entries[0].isVisible===false);visibility(s);},{threshold:0,...('isVisible' in IntersectionObserverEntry.prototype?{trackVisibility:true,delay:100}:{})});s.intersection.observe(el.remote);
    if(PerformanceObserver.supportedEntryTypes.includes('longtask')) {s.performanceObserver=new PerformanceObserver((list)=>{for(const x of list.getEntries())s.longTasks.push({atMs:x.startTime,endMs:x.startTime+x.duration,durationMs:x.duration});});s.performanceObserver.observe({type:'longtask'});}
    let due=performance.now()+100;s.delayTimer=setInterval(()=>{const now=performance.now();if(now-due>50)s.longTasks.push({atMs:due,endMs:now,durationMs:now-due,source:'event-loop delay'});due=now+100;},100);
  }
  function attach(s) {
    for (const [event,kind,key] of [['connectionstatechange','connection','connectionState'],['iceconnectionstatechange','ice','iceConnectionState'],['signalingstatechange','signaling','signalingState']]) {
      s.pc.addEventListener(event,() => {
        note(s,kind,s.pc[key]); descriptions(s);
        if (s.hold?.status==='pending' && kind==='connection' && s.pc.connectionState!=='connected') s.hold.stayedConnected=false;
      });
      note(s,kind,s.pc[key]);
    }
    s.pc.addEventListener('negotiationneeded',() => { if(s.armed) s.negotiations.push(stamp()); });
    const restart=s.pc.restartIce.bind(s.pc);
    s.pc.restartIce=() => { if(s.armed) s.restartCalls.push(stamp()); return restart(); };
    const remote=new MediaStream();el.remote.srcObject=remote;
    s.pc.ontrack=(e) => {
      if (!active(s)) return;
      remote.addTrack(e.track);if(e.track.kind==='audio')s.remoteAudioTrack=e.track;
      el.remote.muted=!el.audio.checked;
      el.remote.play().then(() => {s.playback=el.remote.muted?'muted':'playing';},(err)=>{s.playback=`blocked: ${err.message}; click Play echoed audio`;});
    };
    const decodeCanvas=document.createElement('canvas');decodeCanvas.width=16;decodeCanvas.height=2;
    const decode=decodeCanvas.getContext('2d',{willReadFrequently:true});
    const frame=(now,meta)=>{
      if(!active(s))return;
      const at=typeof meta.presentationTime==='number'?meta.presentationTime:now;
      s.frames.push(at);s.lastVideo=at;
      s.frameMeta.push({atMs:now,presentationTimeMs:at,startMs:s.previousFrameAtMs ?? now,endMs:now,presentedFrames:meta.presentedFrames,starved:M.starved(s.presentedFrames,meta.presentedFrames)});s.presentedFrames=meta.presentedFrames;s.previousFrameAtMs=now;
      {
        let counter=M.UNKNOWN,rawValues=[];
        try {
          const read=M.readCounterBand(decode,el.remote);counter=read.counter;rawValues=read.rawValues;
          if(counter===M.UNKNOWN)throw new Error('counter unreadable');
          // Unwrap 16 bits for long runs. Cached old frames retain their era.
          if(s.maxCounter!==null){counter+=Math.floor(s.maxCounter/65536)*65536;if(counter<s.maxCounter-32768)counter+=65536;else if(counter>s.maxCounter+32768)counter-=65536;}
        }catch(err){
          counter=M.UNKNOWN;
          if(now-(s.lastCounterReadLogMs ?? -Infinity)>=1000){
            const failure={atMs:now,rawValues,error:String(err)};s.counterReadFailures.push(failure);console.warn('counter readback',failure);s.lastCounterReadLogMs=now;
          }
        }
        // Counter pixels are observed now; older compositor metadata must not
        // timestamp a newer snapshot as recovery before it was seen. Keep the
        // presentation time separately as the next interval's conservative start.
        const observation={atMs:now,presentationTimeMs:at,sourceCounterAtObservation:s.sourceCounter ?? M.UNKNOWN,...M.counterObservation(counter,s.maxCounter)};s.contentFrames.push(observation);
        if(observation.advanced){s.maxCounter=counter;s.lastContent=at;}
      }
      s.frameCallback=el.remote.requestVideoFrameCallback(frame);
    };
    s.frameCallback=el.remote.requestVideoFrameCallback(frame);
  }
  async function waitFor(s,predicate,timeout,message) {
    const end=performance.now()+timeout;
    while(active(s) && !predicate()) {
      if(performance.now()>end) throw new Error(message);
      if(['failed','closed'].includes(s.pc.connectionState)) throw new Error(`connection ${s.pc.connectionState}`);
      await sleep(50);
    }
    if(!active(s)) throw new Error('call ended');
  }
  async function start({source=el.source.value,baselineCount=5,headroomRatio=0.8}={}) {
    if(starting) return starting;
    if(call && !call.stopped) { if(!call.armed) throw new Error('call is not ready');return snapshot(call); }
    if(!Number.isInteger(baselineCount) || baselineCount<1)throw new Error('baselineCount must be a positive integer');
    if(!Number.isFinite(headroomRatio) || headroomRatio<=0 || headroomRatio>1)throw new Error('headroomRatio must be greater than zero and at most one');
    if(source==='test')source='pattern';
    if(!['pattern','camera'].includes(source)) throw new Error('source must be pattern (or test) or camera');
    el.source.value=source;
    const s={source,baselineCount,headroomRatio,pc:new RTCPeerConnection({bundlePolicy:'max-bundle',rtcpMuxPolicy:'require'}),...stamp(),startedAt:new Date().toISOString(),
      transitions:[],owners:[],sourceFrames:[],frames:[],frameMeta:[],contentFrames:[],counterReadFailures:[],hiddenPeriods:[],longTasks:[],baselines:[],samples:[],events:[],descriptionChanges:[],negotiations:[],restartCalls:[],sampleErrors:[],lastVideo:null,lastAudio:null,lastContent:null,maxCounter:null,armed:false,stopped:false};
    call=s;el.error.textContent='';observePage(s);attach(s);statuses(s);samples(s);
    starting=(async()=>{
      try {
        const stream=await sourceStream(s);
        if(!active(s)){stream.getTracks().forEach((t)=>t.stop());throw new Error('call ended');}
        s.stream=stream;el.local.srcObject=stream;
        for(const kind of ['audio','video']) {const track=stream.getTracks().find((t)=>t.kind===kind);if(!track) throw new Error(`missing ${kind} source`);s.pc.addTransceiver(track,{direction:'sendrecv',streams:[stream]});}
        await s.pc.setLocalDescription(await s.pc.createOffer());
        if(!active(s)) throw new Error('call ended');
        await waitFor(s,()=>s.pc.iceGatheringState==='complete',10000,'initial ICE gathering did not finish');
        const response=await request('/calls',{method:'POST',headers:{'Content-Type':'application/sdp'},body:s.pc.localDescription.sdp});
        const location=response.headers.get('Location'),body=await response.text();
        if(response.status!==201) throw new Error(`POST /calls ${response.status}: ${body}`);
        if(!location || !/^\/calls\/[^/]+$/.test(location)) throw new Error('invalid call Location');
        s.resource=location;s.id=decodeURIComponent(location.split('/').at(-1));
        if(!active(s)){await request(location,{method:'DELETE'});throw new Error('call ended');}
        await s.pc.setRemoteDescription({type:'answer',sdp:body});
        await waitFor(s,()=>s.pc.connectionState==='connected' && s.lastVideo!==null && s.latest?.audio.packetsReceived>0 && s.owner,15000,'media not ready; test tone may need a page click for Chrome autoplay');
        descriptions(s);s.armed=true;s.readyAtMs=performance.now();return snapshot(s);
      }catch(err){error(err);await cleanup(s);throw err;}finally{starting=null;render();}
    })();
    return starting;
  }
  function measurement(s,begin,end,issued=begin,sourceCounterAtIssue=M.UNKNOWN) {
    const before=s.samples.filter((x)=>x.atMs<=begin).at(-1);
    const after=s.samples.filter((x)=>x.atMs<=end).at(-1);
    const presentation=M.gap(s.frames,begin,end);
    const video={method:'decoded counter',...M.content(s.contentFrames,begin,end,issued,sourceCounterAtIssue)};
    const evidenceStart=typeof video.maxStartMs==='number'?Math.min(begin,video.maxStartMs):begin;
    const source=M.sourceDiagnostics(s.sourceFrames,evidenceStart,end);
    const starvedPeriods=s.frameMeta.filter((x)=>x.starved && x.startMs<=end && x.endMs>=evidenceStart);
    return {
      video,presentation,...source,
      audio:{...M.concealment(s.samples,begin,end,s.sampleRate),lastPacketAgeMs:typeof after?.audio.lastPacketReceivedTimestamp==='number'?end-rtcTime(after.audio.lastPacketReceivedTimestamp):M.UNKNOWN},audioDelta:M.delta(before?.audio || {},after?.audio || {}),videoDelta:M.delta(before?.video || {},after?.video || {}),
      hidden:M.hiddenDuring(s.hiddenPeriods,begin,end),starved:source.sourceStarvedPeriods.length>0 || starvedPeriods.length>0 || s.longTasks.some((x)=>x.atMs<=end && x.endMs>=evidenceStart),
      starvedPeriods,
      unreadPeriods:s.contentFrames.flatMap((f,i,list)=>f.counter===M.UNKNOWN?[{startMs:list[i-1]?.atMs ?? f.atMs,endMs:list[i+1]?.atMs ?? end}]:[]).filter((p)=>p.startMs<=end && p.endMs>=evidenceStart),
      longTasks:s.longTasks.filter((x)=>x.atMs<=end && x.endMs>=evidenceStart),samplingErrors:s.sampleErrors.filter((x)=>x.atMs>=begin && x.atMs<=end)};
  }
  function updateHold(s) {
    const h=s.hold,end=s.latest?.atMs ?? h.startedMs,m=measurement(s,h.startedMs,end);
    h.windowEndMs=end;
    for(const kind of ['video','audio'])if(typeof m[kind].maxMs==='number' && m[kind].maxMs>h.runningMax[kind]){h.runningMax[kind]=m[kind].maxMs;h.largestGaps[kind]={...m[kind]};}
    h.freezeDelta=m.videoDelta.freezeCount;h.freezeDurationDelta=m.videoDelta.totalFreezesDuration;
    h.hidden ||= m.hidden;h.starved ||= m.starved;h.measurement=m;
    if(end>=h.dueMs)finishHold(s);
  }
  function finishHold(s) {
    const h=s.hold;if(!h || h.status!=='pending')return;
    h.observedMs=(h.windowEndMs ?? h.startedMs)-h.startedMs;if(h.observedMs<60000)return;
    const end=h.windowEndMs,m=h.measurement || measurement(s,h.startedMs,end);
    h.checkedAt=new Date().toISOString();
    const record={...m,kind:'baseline',connectionStateBefore:'connected',connectionStateAfter:s.pc.connectionState,
      stateChanges:s.transitions.filter((x)=>x.atMs>=h.startedMs && x.atMs<=end),descriptionChanges:s.descriptionChanges.filter((x)=>x.atMs>=h.startedMs && x.atMs<=end),
      iceRestarts:s.restartCalls.filter((x)=>x.atMs>=h.startedMs && x.atMs<=end).length,renegotiations:s.negotiations.filter((x)=>x.atMs>=h.startedMs && x.atMs<=end).length};
    record.hidden ||= h.hidden;record.starved ||= h.starved;
    if(h.largestGaps.video)for(const key of ['maxMs','maxStartMs','maxEndMs','maxEnded'])record.video[key]=h.largestGaps.video[key];
    record.audio.maxMs=typeof record.audio.maxMs==='number'?Math.max(record.audio.maxMs,h.runningMax.audio):M.UNKNOWN;
    h.gapDiagnostics=M.gapDiagnostics(record);
    h.verdict=M.verdict(record,2000);h.status=h.verdict.status;
  }
  function scheduleHold(s) {
    clearTimeout(s.holdTimer);const at=performance.now();
    s.hold={status:'pending',startedAt:new Date().toISOString(),startedMs:at,dueMs:at+60000,stayedConnected:s.pc.connectionState==='connected',runningMax:{video:0,audio:0},largestGaps:{}};
    s.holdTimer=setTimeout(()=>{if(active(s) && s.hold.status==='pending')updateHold(s);},60000);
  }
  async function event(kind) {
    const s=call;
    if(!s || !s.armed || s.pc.connectionState!=='connected') throw new Error('start a connected call first');
    if(s.busy) throw new Error('an action is already in progress');
    s.busy=true;el.error.textContent='';clearTimeout(s.holdTimer);
    if(s.hold) s.hold.status='superseded';
    try {
      await waitFor(s,()=>performance.now()-s.readyAtMs>=1200,2000,'media lookback not ready');
      await sample(s);
      const issued=performance.now(), begin=M.windowStart(issued,s.previousWindowEnd);
      const before=s.samples.filter((sample)=>sample.atMs<=begin).at(-1) || s.latest;
      if(!before) throw new Error('no inbound stats');
      const record={n:(kind==='baseline'?s.baselines:s.events).length+1,kind,...stamp(),from:s.owner,to:null,issuedAtMs:issued,sourceCounterAtIssue:s.sourceCounter ?? M.UNKNOWN,noiseFloor:M.noiseFloor(s.baselines,s.baselineCount),windowStartMs:begin,connectionStateBefore:s.pc.connectionState};
      try {
        if(kind==='baseline'){record.to=record.from;}else record.server=await json(kind==='move'?`${s.resource}/move`:`/demo/${kind}`,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({id:s.id})});
        if(record.server){record.from=record.server.from || record.from;record.to=record.server.to || record.server.drain?.moves.find((m)=>m.id===s.id)?.to;}
      }catch(err){record.error=String(err);record.server=err.server || null;}
      record.responseAtMs=performance.now();
      await sleep(2000);
      if(active(s))try {
        await sample(s);
        // Close both media windows on the receiver's stats clock. Cached stats
        // must cover the entire two-second settle, not leave an unseen tail.
        await waitFor(s,()=>s.latest?.atMs>=record.responseAtMs+2000,1000,'stats did not cover the measurement window');
      }catch(err){s.sampleErrors.push({...stamp(),error:String(err)});}
      const end=s.latest?.atMs>=issued?s.latest.atMs:performance.now();
      record.windowEndMs=end;record.windowMs=end-begin;record.requestMs=record.responseAtMs-issued;
      Object.assign(record,measurement(s,begin,end,issued,record.sourceCounterAtIssue));
      record.contentResumedMs=record.video.contentResumedMs ?? M.UNKNOWN;
      record.gapDiagnostics=M.gapDiagnostics(record);
      record.noiseGate=M.noiseGate(record.noiseFloor,s.baselineCount,100,s.headroomRatio);
      Object.assign(record,M.noiseDiagnostics(record,record.frameIntervalMs));
      record.timeToFirstNewContentMs=record.video.firstNewContentMs ?? M.UNKNOWN;
      record.timeToFirstLiveFrameMs=M.firstLiveFrame(kind,s.contentFrames,issued,end,record.video.contentResumedMs);
      s.previousWindowEnd=end;
      record.connectionStateAfter=s.pc.connectionState;
      record.samplingErrors=s.sampleErrors.filter((e)=>e.atMs>=begin && e.atMs<=end);
      record.stateChanges=s.transitions.filter((e)=>e.atMs>=begin && e.atMs<=end);
      record.descriptionChanges=s.descriptionChanges.filter((e)=>e.atMs>=begin && e.atMs<=end);
      record.renegotiations=s.negotiations.filter((e)=>e.atMs>=begin && e.atMs<=end).length;
      record.iceRestarts=s.restartCalls.filter((e)=>e.atMs>=begin && e.atMs<=end).length+record.descriptionChanges.filter((e)=>e.iceRestart).length;
      record.windowVerdict=M.verdict(record);record.windowPass=record.windowVerdict.status==='pass'?true:record.windowVerdict.status==='fail'?false:null;
      (kind==='baseline'?s.baselines:s.events).push(record);
      if(active(s)) scheduleHold(s);
      else s.hold={status:'fail',verdict:{status:'fail',reasons:['call ended during event']}};
      return kind==='baseline'?record:snapshot(s).events.at(-1);
    }finally{s.busy=false;render();}
  }
  async function run(kind,n,spacingMs=3000) {
    if(!Number.isInteger(n) || n<1 || !Number.isFinite(spacingMs) || spacingMs<0) throw new Error('positive integer count and nonnegative spacing required');
    const s=call;if(!s || s.script || s.busy) throw new Error('call missing or another action is running');
    s.script=kind;
    const records=[];
    try {
      // Establish a full, media-bearing lookback for the first event.
      await sleep(Math.max(0,1200-(performance.now()-s.readyAtMs)));
      for(let i=0;i<n;i++) {
        if(!active(s)) throw new Error('call ended during scripted run');
        const at=performance.now();records.push(await event(kind));
        if(i<n-1) await sleep(Math.max(0,spacingMs-(performance.now()-at)));
      }
      return records;
    }finally{s.script=null;render();}
  }
  async function waitForLongHold() {
    const s=call;if(!s?.hold) throw new Error('run an event first');
    while(active(s) && s.hold.status==='pending') await sleep(100);
    return snapshot(s).longHold;
  }
  async function cleanup(s) {
    if(s.stopped)return;
    if(s.hold?.status==='pending') {const hidden=M.hiddenDuring(s.hiddenPeriods,s.hold.startedMs,performance.now());s.hold.status=hidden?'invalid':'inconclusive';s.hold.verdict={status:s.hold.status,reasons:[hidden?'page hidden':'call stopped before 60 s']};}
    s.stoppedAtMs=performance.now();s.stopped=true;clearInterval(s.sourceTimer);clearTimeout(s.holdTimer);
    if(s.frameCallback!==undefined) el.remote.cancelVideoFrameCallback(s.frameCallback);
    clearInterval(s.delayTimer);s.performanceObserver?.disconnect();s.intersection?.disconnect();document.removeEventListener('visibilitychange',s.visibilityListener);
    s.pc.close();note(s,'connection','closed');note(s,'ice','closed');
    if(s.cameraCallback!==undefined)s.cameraVideo.cancelVideoFrameCallback(s.cameraCallback);
    if(s.cameraVideo){s.cameraVideo.pause();s.cameraVideo.srcObject=null;}
    s.cameraStream?.getTracks().forEach((t)=>t.stop());s.canvasStream?.getTracks().forEach((t)=>t.stop());
    s.stream?.getTracks().forEach((t)=>t.stop());
    if(s.audioContext) await s.audioContext.close();
    if(s.resource) {try{const response=await request(s.resource,{method:'DELETE'});if(!response.ok) throw new Error(`DELETE ${response.status}`);}catch(err){s.cleanupError=String(err);error(err);}}
    lastResult=snapshot(s);render();
  }
  async function stop() {if(call)await cleanup(call);return snapshot(call);}
  async function runChecklist() {
    const s=call;if(!s || !s.armed || s.checklist || s.script || s.busy)throw new Error('start a ready call with no other action first');
    s.checklist=true;
    const ensureCall=()=>{if(!active(s))throw new Error('call ended during checklist');};
    try {
      await run('baseline',s.baselineCount);ensureCall();
      await run('move',10);ensureCall();
      await run('kill',10);ensureCall();
      await waitForLongHold();ensureCall();
    }
    finally{s.checklist=false;render();}
    return snapshot(s);
  }
  function saveResults() {
    const record=snapshot(call),blob=new Blob([JSON.stringify(record,null,2)+'\n'],{type:'application/json'});
    const url=URL.createObjectURL(blob),a=document.createElement('a');
    a.href=url;a.download=`relais-${record.source || 'idle'}-${new Date().toISOString().replace(/[:.]/g,'-')}.json`;
    a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);return record;
  }
  el.source.value=params.get('source')==='camera'?'camera':'pattern';
  window.relaisDemo={runChecklist,saveResults,start,runBaseline:(n=call?.baselineCount || 5,ms)=>run('baseline',n,ms),runMoves:(n,ms)=>run('move',n,ms),runKills:(n,ms)=>run('kill',n,ms),runDrains:(n,ms)=>run('drain',n,ms),results:()=>snapshot(call),waitForLongHold,stop};
  el.save.onclick=saveResults;el.check.onclick=()=>runChecklist().catch(error);
  el.start.onclick=()=>start().catch(error);el.stop.onclick=()=>stop().catch(error);
  for(const kind of ['move','drain','kill']) el[kind].onclick=()=>event(kind).catch(error);
  el.audio.onchange=()=>{el.remote.muted=!el.audio.checked;el.remote.play().then(()=>{if(call)call.playback=el.remote.muted?'muted':'playing';},error);if(call && !call.stopped && call.audioContext?.state==='suspended') call.audioContext.resume().catch(error);};
  document.addEventListener('pointerdown',()=>{if(call && !call.stopped && call.audioContext?.state==='suspended') call.audioContext.resume().catch(error);});
  window.addEventListener('pagehide',()=>{if(call?.resource)request(call.resource,{method:'DELETE',keepalive:true}).catch(()=>{});});
  setInterval(render,1000);render();
  if(params.get('autostart')==='1')start().catch(error);
})();
