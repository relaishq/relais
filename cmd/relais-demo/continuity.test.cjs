const {test}=require('node:test');
const assert=require('node:assert/strict');
const A=require('./web/continuity.js');
const p=(n,phase=0)=>({count:n,phase_samples:phase});
const progress=(n)=>({SSRC:123,Index:n,Timestamp:n*960,Consumed:n});
function evidence(kind='move') {
 const cp={position:p(3,9600),progress:progress(60)};
 const resume={kind:kind==='kill'?'takeover':'planned_move',number:2,...cp,checkpoint_age_ms:180,snapshot_age_ms:200,input_may_be_duplicated:kind==='kill',duplicate_windows:[]};
 return {before:{position:p(3,14400),last_resume:{number:1}},after:{position:p(3,9600),checkpoint:cp,last_resume:resume},exported:{...cp},measured_checkpoint_age_ms:180,measured_snapshot_age_ms:200,errors:[],observed_after:{position:p(7),progress:progress(160),last_resume:resume}};
}
test('exact planned count and tone phase pass, plus drain',()=>{
 assert.equal(A.verdict('move',evidence()).status,'pass');
 assert.equal(A.verdict('drain',evidence()).status,'pass');
 const e=evidence();e.exported={position:p(3,8640),progress:progress(59)};
 assert.equal(A.verdict('move',e).status,'fail');
});
test('takeover uses actual checkpoint and measured snapshot age including write delay',()=>{
 const e=evidence('kill');
 assert.equal(A.verdict('kill',e).status,'pass');
 assert.equal(A.verdict('kill',e).inputMayBeDuplicated,true);
 e.before.position=p(4,9600);
 assert.equal(A.verdict('kill',e).status,'fail');
 const delayed=evidence('kill');delayed.before.position=p(3,19200);delayed.measured_checkpoint_age_ms=10;delayed.after.last_resume.checkpoint_age_ms=10;
 assert.equal(A.verdict('kill',delayed).status,'pass');
});
test('missing evidence never passes',()=>{
 for(const key of ['before','after','observed_after','exported']) {
  const e=evidence();delete e[key];assert.equal(A.verdict('move',e).status,'inconclusive');
 }
 assert.equal(A.verdict('kill',null).status,'inconclusive');
 const e=evidence('kill');delete e.measured_snapshot_age_ms;assert.equal(A.verdict('kill',e).status,'inconclusive');
 const errors=evidence();errors.errors=['status timeout'];assert.equal(A.verdict('move',errors).status,'inconclusive');
});
test('corrupt restore, stale resume, frozen agent and age mismatch fail',()=>{
 for(const change of [e=>e.after.last_resume={...e.after.last_resume,position:p(3,8640)},e=>e.after.last_resume={...e.after.last_resume,progress:progress(59)},e=>e.after.last_resume={...e.after.last_resume,number:1},e=>e.observed_after={...e.observed_after,position:p(3,9600)},e=>e.observed_after={...e.observed_after,progress:progress(60)},e=>e.after.last_resume={...e.after.last_resume,input_may_be_duplicated:true}]) {
  const e=evidence();change(e);assert.equal(A.verdict('move',e).status,'fail');
 }
 const e=evidence('kill');e.measured_checkpoint_age_ms=200;assert.equal(A.verdict('kill',e).status,'fail');
});
