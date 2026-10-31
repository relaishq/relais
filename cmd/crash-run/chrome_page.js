'use strict';
window.chromeGate={calls:[],error:'',failure:null};
chromeGate.setupState=index=>{
 const c=chromeGate.calls[index];
 return {index:c.index,path:c.path,ready:c.ready,ended:c.ended,phase:c.phase,phases:c.phases,failure:c.failure};
};
// Timestamp the observation before CDP serializes it. A later clock read would
// count transfer time as frozen media, increasingly so as history accumulates.
chromeGate.snapshot=(index,since)=>{
 const c=chromeGate.calls[index],at=performance.now();
 const first=c.samples.findIndex(s=>s.at>since);
 const counters=t=>({packetsReceived:t.packetsReceived,framesDecoded:t.framesDecoded,bytesReceived:t.bytesReceived,rtpTimestamp:t.rtpTimestamp,packetsFailedDecryption:t.packetsFailedDecryption});
 return {at,error:chromeGate.error,path:c.path,ended:c.ended,reconnects:c.reconnects,
  samples:c.samples.slice(first<0?-1:Math.max(0,first-1)).map(s=>({...s,audio:counters(s.audio),video:counters(s.video)})),timeline:c.timeline.filter(e=>e.at>=since)};
};
const chromeTimeline=(gate,event,detail={})=>gate.timeline.push({at:performance.now(),event,...detail});
const chromePhase=(gate,phase)=>{
 gate.phase=phase;gate.phases.push({phase,at:performance.now()});
 console.info('CHROME_PHASE',JSON.stringify({call:gate.index,path:gate.path || null,phase}));
};
const chromeFailure=(gate,err,phase=gate.phase)=>{
 const failure={call:gate.index,path:gate.path || null,phase,message:String(err),stack:err?.stack || ''};
 gate.failure=failure;gate.ended=failure.message;
 chromeGate.error=failure.message;chromeGate.failure=failure;
 console.error('CHROME_JS_FAILURE',JSON.stringify(failure));
};
chromeGate.addCall=async()=>{
 const gate={index:chromeGate.calls.length,samples:[],timeline:[],ended:'',reconnects:0,ready:false,phase:'create-peer',phases:[],failure:null};chromeGate.calls.push(gate);
 try {
  chromePhase(gate,'create-peer');
  const pc=new RTCPeerConnection({bundlePolicy:'max-bundle'});gate.pc=pc;
  for(const method of ['restartIce','createOffer','setLocalDescription','setRemoteDescription']){
   const original=pc[method];if(typeof original!=='function')continue;
   pc[method]=function(...args){
    chromeTimeline(gate,method+'-start');
    const result=original.apply(this,args);
    if(result?.then)return result.then(value=>{chromeTimeline(gate,method+'-done');return value;});
    chromeTimeline(gate,method+'-done');return result;
   };
  }
  for(const event of ['icegatheringstatechange','iceconnectionstatechange'])pc.addEventListener(event,()=>chromeTimeline(gate,event,{gathering:pc.iceGatheringState,ice:pc.iceConnectionState}));
  let connected=false;
  pc.addEventListener('connectionstatechange',()=>{if(pc.connectionState==='connected'){if(connected)gate.reconnects++;connected=true;}});
  pc.ontrack=({track,streams})=>{
   try {
    if(track.kind==='video'){const v=document.createElement('video');v.autoplay=true;v.muted=true;v.srcObject=streams[0] || new MediaStream([track]);document.body.append(v);}
   }catch(err){chromeFailure(gate,err,'attach-remote-track');}
  };
  chromePhase(gate,'get-user-media');
  const media=await navigator.mediaDevices.getUserMedia({audio:true,video:{width:320,height:240,frameRate:30}});
  chromePhase(gate,'add-transceivers');
  for(const track of media.getTracks())pc.addTransceiver(track,{direction:'sendrecv',streams:[media]});
  chromePhase(gate,'create-offer');
  const offer=await pc.createOffer();
  chromePhase(gate,'set-local-description');
  await pc.setLocalDescription(offer);
  chromePhase(gate,'gather-ice');
  while(pc.iceGatheringState!=='complete')await new Promise(r=>setTimeout(r,10));
  chromePhase(gate,'post-call');
  const relayChangeCapable=typeof RTCPeerConnection.prototype.restartIce==='function';
  const response=await fetch('/calls',{method:'POST',headers:{'Content-Type':'application/sdp',...(relayChangeCapable?{'Relais-Relay-Change':'true'}:{})},body:pc.localDescription.sdp});
  chromePhase(gate,'read-answer');
  const answer=await response.text();
  if(response.status!==201)throw Error(answer);
  gate.path=response.headers.get('Location');
  chromePhase(gate,'set-remote-description');
  await pc.setRemoteDescription({type:'answer',sdp:answer});
  for(const ice of new Set(pc.getSenders().map(sender=>sender.transport?.iceTransport).filter(Boolean))){
   ice.addEventListener('selectedcandidatepairchange',()=>{
    const pair=ice.getSelectedCandidatePair();
    chromeTimeline(gate,'selected-candidate-pair',{remote:pair?.remote.address+':'+pair?.remote.port,source:'RTCIceTransport.selectedcandidatepairchange'});
   });
  }
  chromePhase(gate,'create-relay-controller');
  if(relayChangeCapable){
   gate.controller=new relaisRelayChange.Controller({pc,path:gate.path,
    events:url=>{
     const stream=new EventSource(url);
     stream.addEventListener('relay-change',event=>chromeTimeline(gate,'relay-change-event',{generation:JSON.parse(event.data).generation}));
     return stream;
    },
    request:async(url,options)=>{
     chromeTimeline(gate,'restart-post-start');
     const response=await fetch(url,options);
     chromeTimeline(gate,'restart-post-headers',{status:response.status});
     return {ok:response.ok,status:response.status,text:async()=>{const body=await response.text();chromeTimeline(gate,'restart-answer-read');return body;}};
    },end:reason=>{gate.ended=reason;pc.close();}});
   chromePhase(gate,'open-relay-controller');
   gate.controller.open();
  }
  const checkedPairs=new Set();let pairProgress;
  const sample=async()=>{
   const started=performance.now();
   gate.phase='stats-getStats';
   const report=await pc.getStats();
   const statsDone=performance.now();
   gate.phase='stats-observe';
   const all=[...report.values()];gate.controller?.observe(all);
   const inbound=kind=>all.find(x=>x.type==='inbound-rtp' && x.kind===kind) || {};
   const audio=inbound('audio');
   const receiver=pc.getReceivers().find(x=>x.track.kind==='audio');
   const sync=receiver?.getSynchronizationSources().find(x=>x.source===audio.ssrc);
   const sources={audioPackets:'RTCInboundRtpStreamStats.packetsReceived',audioRTP:sync?'RTCRtpReceiver.getSynchronizationSources().rtpTimestamp':null,videoFrames:'RTCInboundRtpStreamStats.framesDecoded'};
   const transport=all.find(x=>x.type==='transport' && x.selectedCandidatePairId),pair=report.get(transport?.selectedCandidatePairId),remote=report.get(pair?.remoteCandidateId);
   const video=inbound('video'),address=remote?.address+':'+remote?.port;
   for(const p of all.filter(x=>x.type==='candidate-pair')){
    if(p.requestsSent>0 && !checkedPairs.has(p.id)){
     checkedPairs.add(p.id);const r=report.get(p.remoteCandidateId);
     chromeTimeline(gate,'first-check-observed',{pair:p.id,remote:r?.address+':'+r?.port,requestsSent:p.requestsSent,source:'RTCIceCandidatePairStats.requestsSent'});
    }
   }
   if(pair && pairProgress?.id!==pair.id){
    pairProgress={id:pair.id,audio:audio.packetsReceived,video:video.framesDecoded};
    chromeTimeline(gate,'stats-pair-selected',{pair:pair.id,remote:address});
   }
   if(pairProgress && !pairProgress.received && pair?.bytesReceived>0){pairProgress.received=true;chromeTimeline(gate,'pair-received-bytes-observed',{remote:address,bytes:pair.bytesReceived});}
   if(pairProgress && !pairProgress.audioAdvanced && audio.packetsReceived>pairProgress.audio){pairProgress.audioAdvanced=true;chromeTimeline(gate,'audio-progress-new-pair',{remote:address,packets:audio.packetsReceived,rtpTimestamp:sync?.rtpTimestamp});}
   if(pairProgress && !pairProgress.videoAdvanced && video.framesDecoded>pairProgress.video){pairProgress.videoAdvanced=true;chromeTimeline(gate,'video-decoded-new-pair',{remote:address,frames:video.framesDecoded});}
   const at=performance.now(),previous=gate.samples.at(-1);
   gate.samples.push({at,audio:{...audio,rtpTimestamp:sync?.rtpTimestamp},video,sources,
    sampling:{statsMs:statsDone-started,processMs:at-statsDone,intervalMs:previous?at-previous.at:0,reportCount:all.length,pairCount:all.filter(x=>x.type==='candidate-pair').length},
    pair:pair?{id:pair.id,remote:address,packetsSent:pair.packetsSent,bytesReceived:pair.bytesReceived}:null});
   gate.phase='sampling';
   if(!gate.ended)setTimeout(()=>sample().catch(err=>chromeFailure(gate,err)),10);
  };
  await sample();gate.ready=true;chromePhase(gate,'sampling');
 }catch(err){chromeFailure(gate,err);throw err;}
};
chromeGate.addCall().catch(()=>{});
//# sourceURL=chrome-relay-caller.js
