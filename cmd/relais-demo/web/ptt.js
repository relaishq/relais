'use strict';
(function(root) {
  const definite=new Set('move_budget agent_incompatible agent_oversize agent_restore agent_save agent_version buffer_disabled control_conflict control_invalid control_order control_stale control_unsupported ended export_incompatible not_found sequence_budget srtcp_exhausted unknown_call unsupported_offer'.split(' '));
  const retryable=new Set('agent_backpressure agent_capacity barrier_timeout canceled checkpoint_clock closed control_not_ready control_rejected deadline export_not_flushed held hold_expired hold_limit lease_lost move_in_progress no_target not_established replay_in_progress replay_not_ready replay_target_changed session_exists transient unknown_session'.split(' '));
  const classify=(code)=>definite.has(code)?'definite':retryable.has(code)?'retryable':'uncertain';
  class Controller {
    constructor({request,path,notice=()=>{},now=()=>performance.now(),sleep=(ms)=>new Promise((resolve)=>setTimeout(resolve,ms)),id=()=>crypto.randomUUID(),deadlineMs=15000}) {
      Object.assign(this,{request,path,notice,now,sleep,id,deadlineMs});this.sequence=0;this.local=Promise.resolve();this.failed=false;this.closed=false;
    }
    close() {this.closed=true;}
    send(name) {
      const message={id:this.id(),sequence:++this.sequence,name};
      let accept,reject;const local=new Promise((yes,no)=>{accept=yes;reject=no;});
      const previous=this.local;this.local=local;local.catch(()=>{});
      const completed=(async()=>{
        await previous;
        if(this.failed || this.closed)throw new Error('Control stopped; start a new call.');
        const body=JSON.stringify(message),due=this.now()+this.deadlineMs;let delay=50,accepted=false;
        while(!this.closed && this.now()<due) {
          let response,data;
          try {
            response=await this.request(this.path,{method:'POST',headers:{'Content-Type':'application/json'},body,signal:AbortSignal.timeout(Math.min(2000,Math.max(1,due-this.now())))});
            const text=await response.text();try {data=JSON.parse(text);} catch {data=response.status===400?{code:'control_invalid'}:{};}
          }catch {data={code:'uncertain'};}
          const receipt=(response?.status===200 || response?.status===202) && data?.id===message.id && data?.sequence===message.sequence;
          if(receipt || data?.code==='control_stale') {
            if(!accepted){accepted=true;accept();}
            if(data.code==='control_stale' || response.status===200 && data.checkpointed===true)return data;
          }else if(classify(data?.code)==='definite')throw new Error(`Press refused (${data.code}). Start a new call.`);
          await this.sleep(Math.min(delay,Math.max(0,due-this.now())));delay=Math.min(1000,delay*2);
        }
        throw new Error('Control acknowledgement timed out. The press may have reached the worker; start a new call.');
      })().catch((err)=>{this.failed=true;reject(err);this.notice(err.message);throw err;});
      completed.catch(()=>{});return completed;
    }
  }
  const api={Controller,classify,definite,retryable};if(typeof module!=='undefined')module.exports=api;else root.relaisPTT=api;
})(typeof window==='undefined'?globalThis:window);
