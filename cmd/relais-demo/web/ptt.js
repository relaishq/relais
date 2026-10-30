'use strict';
(function(root) {
 const definite=new Set('relay_switched relay_change_rolled_back restart_mismatch relay_unsupported relay_conflict move_budget agent_incompatible agent_oversize agent_restore agent_save agent_version buffer_disabled control_conflict control_invalid control_order control_stale control_unsupported dtls_exhausted ended export_incompatible not_found sequence_budget srtcp_exhausted unknown_call unsupported_offer'.split(' '));
 const retryable=new Set('agent_backpressure agent_capacity barrier_timeout canceled checkpoint_clock closed control_not_ready control_rejected deadline export_not_flushed held hold_expired hold_limit lease_lost move_in_progress no_target at_capacity not_established replay_in_progress replay_not_ready replay_target_changed session_exists stale_tenure transient unknown_session worker_command_not_sent worker_link_unavailable'.split(' '));
 const notAccepted=new Set(['agent_capacity','control_not_ready','control_rejected','agent_backpressure']);
 const classify=(code)=>definite.has(code)?'definite':retryable.has(code)?'retryable':'uncertain';
 class Controller {
  constructor({request,path,notice=()=>{},now=()=>performance.now(),sleep=(ms)=>new Promise(resolve=>setTimeout(resolve,ms)),id=()=>crypto.randomUUID(),deadlineMs=15000,acceptedDeadlineMs=45000}) {
   Object.assign(this,{request,path,notice,now,sleep,id,deadlineMs,acceptedDeadlineMs});this.sequence=0;this.local=Promise.resolve();this.pending=new Set();this.failed=false;this.closed=false;
  }
  close(){this.closed=true;}
  send(name){
   let accept,reject;const local=new Promise((yes,no)=>{accept=yes;reject=no;});
   const previous=this.local;this.local=local;local.catch(()=>{});
   let reusable=false;
   const completed=(async()=>{
    await previous;
    if(this.failed || this.closed)throw Error('Control stopped; start a new call.');
    // Assign after the preceding message is locally accepted or definitely not
    // accepted. Only that latter case can safely reuse its sequence with a new ID.
    const message={id:this.id(),sequence:this.sequence+1,name};
    const body=JSON.stringify(message);let due=this.now()+this.deadlineMs,delay=50,accepted=false,onlyRefusals=true;
    while(!this.closed && this.now()<due){
     let response,data;
     try{
      response=await this.request(this.path,{method:'POST',headers:{'Content-Type':'application/json'},body,signal:AbortSignal.timeout(Math.min(2000,Math.max(1,due-this.now())))});
      const text=await response.text();try{data=JSON.parse(text);}catch{data=response.status===400?{code:'control_invalid'}:{};}
     }catch{data={code:'uncertain'};}
     if(response?.status===401 || response?.status===403)throw Error('Press refused. Open the link printed by relais-demo.');
     const receipt=(response?.status===200 || response?.status===202) && data?.id===message.id && data?.sequence===message.sequence;
     if(receipt || data?.code==='control_stale'){
      if(!accepted){
       accepted=true;this.sequence=message.sequence;this.pending.add(message.sequence);accept();
       // ADR 0008 permits a 30 s outage; allow recovery plus acknowledgement.
       due=Math.max(due,this.now()+Math.max(45000,this.acceptedDeadlineMs));
      }
      if(data.code==='control_stale' || response.status===200 && data.checkpointed===true){this.pending.delete(message.sequence);return data;}
     }else{
      const earlierUnconfirmed=[...this.pending].some(sequence=>sequence<message.sequence);
      const ordering=data?.code==='control_order' && earlierUnconfirmed;
      if(!notAccepted.has(data?.code) && !ordering)onlyRefusals=false;
      if(!ordering && classify(data?.code)==='definite')throw Error(`Press refused (${data.code}). Start a new call.`);
     }
     await this.sleep(Math.min(delay,Math.max(0,due-this.now())));delay=Math.min(1000,delay*2);
    }
    if(!this.closed && !accepted && onlyRefusals){reusable=true;accept();throw Error('Press not accepted. Hold to talk again when the worker is ready.');}
    throw Error('Control acknowledgement timed out. The press may have reached the worker; start a new call.');
   })().catch(err=>{if(!reusable){this.failed=true;reject(err);}this.notice(err.message);throw err;});
   completed.catch(()=>{});return completed;
  }
 }
 const api={Controller,classify,definite,retryable};if(typeof module!=='undefined')module.exports=api;else root.relaisPTT=api;
})(typeof window==='undefined'?globalThis:window);
