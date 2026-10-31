'use strict';
((root)=>{
  // One peer connection, one serialized offer exchange. A refused offer can be
  // answered by a later rollback generation without replacing DTLS or tracks.
  class Controller {
    constructor({pc,path,request,end,events=url=>new EventSource(url),now=()=>performance.now(),sleep=ms=>new Promise(resolve=>setTimeout(resolve,ms)),tick=setInterval,untick=clearInterval}) {
      Object.assign(this,{pc,path,end,events,now,sleep});
      // Native Window functions reject a Controller receiver. Invoke injected
      // functions as plain calls, just as their original global call sites do.
      this.request=(...args)=>request(...args);
      this.tick=(...args)=>tick(...args);
      this.untick=(...args)=>untick(...args);
      this.closed=false;this.generation=0;this.pending=0;this.queue=Promise.resolve();this.lastInbound=null;this.counters=new Map();this.attemptUntil=0;
      this.stallEnabled=true;this.eventBackoff=50;this.reopenAt=null;
      this.iceListener=()=>{void this.check();};
    }
    open(){
      this.connectEvents();
      this.pc.addEventListener('iceconnectionstatechange',this.iceListener);
      this.timer=this.tick(()=>{void this.check();},100);
    }
    connectEvents(){
      const stream=this.events(this.path+'/events');this.stream=stream;this.reopenAt=null;
      stream.addEventListener('open',()=>{if(this.stream===stream)this.eventBackoff=50;});
      stream.addEventListener('error',()=>{
        // CONNECTING streams already retry natively. CLOSED (2) is terminal
        // after HTTP errors; reopen it ourselves without duplicating streams.
        if(this.closed || this.stream!==stream || stream.readyState!==2 || this.reopenAt!==null)return;
        stream.close();this.reopenAt=this.now()+this.eventBackoff;this.eventBackoff=Math.min(2*this.eventBackoff,1000);
      });
      stream.addEventListener('relay-change',event=>{
        try {void this.restart(JSON.parse(event.data));}catch {this.fail('Invalid relay change event.');}
      });
      stream.addEventListener('ended',event=>{
        try {this.fail('Call ended: '+JSON.parse(event.data).reason);}catch {this.fail('Call ended: invalid relay reason.');}
      });
    }
    close(){this.closed=true;this.reopenAt=null;this.stream?.close();this.untick(this.timer);this.pc.removeEventListener('iceconnectionstatechange',this.iceListener);this.abort?.abort();}
    fail(reason){if(this.closed)return;this.close();void this.end(reason);}
    observe(stats){
      for(const stat of stats){
        // remote-inbound reports are RTCP; inbound packets are RTP. Stats
        // timestamps alone can advance even when no packet has arrived.
        const count=stat.type==='inbound-rtp'?stat.packetsReceived:stat.type==='remote-inbound-rtp'?stat.reportsReceived:undefined;
        if(typeof count!=='number')continue;
        if(count>(this.counters.get(stat.id) || 0))this.lastInbound=this.now();
        this.counters.set(stat.id,count);
      }
    }
    async check(){
      if(this.closed)return;
      if(this.reopenAt!==null && this.now()>=this.reopenAt)this.connectEvents();
      if(!this.stallEnabled || this.pending || this.now()<this.attemptUntil)return;
      if(['disconnected','failed'].includes(this.pc.iceConnectionState) || this.lastInbound!==null && this.now()-this.lastInbound>=2000){
        this.attemptUntil=this.now()+2000;
        await this.restart({});
      }
    }
    restart(event){
      this.pending++;
      const task=this.queue.then(()=>this.exchange(event));
      this.queue=task.catch(err=>this.fail('Cannot restart call: '+err.message)).finally(()=>{this.pending--;});
      return this.queue;
    }
    async exchange(event){
      if(this.closed || event.generation && event.generation<=this.generation)return;
      if(typeof this.pc.restartIce!=='function')throw Error('ICE restart is unavailable in this browser.');
      const remaining=event.deadline?Date.parse(event.deadline)-Date.now():15000;
      if(remaining<=0)return;
      const due=this.now()+remaining;
      this.attemptUntil=event.generation?due:this.now()+2000;
      this.abort=new AbortController();
      // Local API errors mean this caller cannot restart. HTTP refusals and
      // transport errors can be repaired by control's next generation.
      if(this.pc.signalingState!=='have-local-offer'){
        this.pc.restartIce();
        await this.pc.setLocalDescription(await this.pc.createOffer());
        while(!this.closed && this.pc.iceGatheringState!=='complete' && this.now()<due)await this.sleep(20);
      }
      if(this.closed)return;
      if(this.now()>=due)return;
      let body,response;
      try {
        response=await this.request(this.path+'/ice-restart'+(event.generation?'?generation='+event.generation:''),{method:'POST',headers:{'Content-Type':'application/sdp'},body:this.pc.localDescription.sdp,signal:AbortSignal.any([this.abort.signal,AbortSignal.timeout(Math.max(1,Math.ceil(due-this.now())))])});
        body=await response.text();
        if(this.closed)return;
      }catch(err){
        if(!this.closed && !(err instanceof TypeError) && err.name!=='AbortError' && err.name!=='TimeoutError')throw err;
        return;
      }
      if(!response.ok){
        // The legacy demo's control has no relay set. Its spontaneous endpoint
        // rejects unsupported restarts before reading the offer with this reply.
        if(!event.generation && response.status===400 && body.trim()==='invalid generation'){
          if(this.pc.signalingState==='have-local-offer')await this.pc.setLocalDescription({type:'rollback'});
          this.stallEnabled=false;
        }
        return;
      }
      await this.pc.setRemoteDescription({type:'answer',sdp:body});
      if(!event.generation)this.attemptUntil=this.now()+2000;
      // A spontaneous request races the SSE for that same generation.
      const match=/^a=ice-ufrag:[^\r\n+]+\+(\d+)\r?$/m.exec(body);
      this.generation=Math.max(this.generation,event.generation || 0,Number(match?.[1] || 0));
    }
  }
  const api={Controller};if(typeof module!=='undefined')module.exports=api;else root.relaisRelayChange=api;
})(typeof window==='undefined'?globalThis:window);
