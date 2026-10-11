const {test}=require('node:test');
const assert=require('node:assert/strict');
const A=require('./web/continuity.js');
const p=(n,phase=0)=>({count:n,phase_samples:phase});
const progress=(n)=>({SSRC:123,Index:n,Timestamp:n*960,Consumed:n});
function evidence(kind='move') {
 const cp={position:p(3,9600),progress:progress(60)};
 const resume={kind:kind==='kill'?'takeover':'planned_move',number:2,...cp,checkpoint_age_ms:180,snapshot_age_ms:200,input_may_be_duplicated:kind==='kill',duplicate_windows:[]};
 return {before:{position:p(3,14400),last_resume:{number:1}},after:{position:p(3,9600),progress:progress(60),checkpoint:structuredClone(cp),last_resume:structuredClone(resume)},exported:structuredClone(cp),measured_checkpoint_age_ms:180,measured_snapshot_age_ms:200,errors:[],observed_after:{position:p(7),progress:progress(160),last_resume:structuredClone(resume)}};
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

test('missing consumed evidence is inconclusive, never a pass',()=>{
 for(const path of [['observed_after','progress'],['observed_after','progress','Consumed']]) {
  const e=evidence();let parent=e;for(const key of path.slice(0,-1))parent=parent[key];delete parent[path.at(-1)];
  assert.equal(A.verdict('move',e).status,'inconclusive',path.join('.'));
 }
});
test('fresh progress is required after the immediate post-resume observation',()=>{
 for(const change of [
  e=>{e.after.position=p(5);e.after.progress=progress(120);e.observed_after.position=p(5);e.observed_after.progress=progress(120);},
  e=>{e.after.position=p(6);e.after.progress=progress(140);e.observed_after.position=p(5);e.observed_after.progress=progress(120);},
  e=>{e.after.progress=progress(180);},
  e=>{e.after.position=p(5);e.after.progress=progress(120);e.observed_after.progress=progress(120);},
  e=>{e.after.progress=progress(59);},
 ]) {const e=evidence();change(e);assert.equal(A.verdict('move',e).status,'fail');}
});

const positionPaths=['before.position','after.position','observed_after.position','after.last_resume.position','after.checkpoint.position'];
const progressPaths=['after.progress','observed_after.progress','after.last_resume.progress','after.checkpoint.progress'];
const positionFields=(paths)=>paths.flatMap((path)=>[path,`${path}.count`,`${path}.phase_samples`]);
const progressFields=(paths)=>paths.flatMap((path)=>[path,...['SSRC','Index','Timestamp','Consumed'].map((key)=>`${path}.${key}`)]);
const commonFields=['errors','before','after','observed_after','after.checkpoint','after.last_resume','before.last_resume.number','observed_after.last_resume','observed_after.last_resume.number',...positionFields(positionPaths),...progressFields(progressPaths),...['kind','number','input_may_be_duplicated','duplicate_windows','checkpoint_age_ms','snapshot_age_ms'].map((key)=>`after.last_resume.${key}`)];
function changeAt(e,path,change) {
 const keys=path.split('.');let parent=e;
 for(const key of keys.slice(0,-1))parent=parent[key];
 change(parent,keys.at(-1));
}
test('every required nested field is removed individually before comparisons',()=>{
 for(const kind of ['move','drain','kill']) {
  const paths=[...commonFields,...(kind==='kill'?['measured_checkpoint_age_ms','measured_snapshot_age_ms']:['exported',...positionFields(['exported.position']),...progressFields(['exported.progress'])])];
  for(const path of paths) {
   const e=evidence(kind);changeAt(e,path,(parent,key)=>delete parent[key]);
   assert.equal(A.verdict(kind,e).status,'inconclusive',`${kind}: ${path}`);
  }
 }
 // No prior resume is the valid initial-call state, not missing evidence.
 const first=evidence();delete first.before.last_resume;first.after.last_resume.number=1;first.observed_after.last_resume.number=1;
 assert.equal(A.verdict('move',first).status,'pass');
});
test('invalid nested field types and nonfinite or out-of-range numbers are inconclusive',()=>{
 for(const kind of ['move','kill']) {
  const fields=[...commonFields,...(kind==='kill'?['measured_checkpoint_age_ms','measured_snapshot_age_ms']:['exported',...positionFields(['exported.position']),...progressFields(['exported.progress'])])];
  for(const path of fields) {
   const numeric=/(count|phase_samples|SSRC|Index|Timestamp|Consumed|number|_age_ms)$/.test(path);
   const values=path.endsWith('_age_ms')?[null,'1',NaN,Infinity,-Infinity,-1,{},[]]:numeric?[null,'1',NaN,Infinity,-Infinity,-1,1.5,Number.MAX_SAFE_INTEGER+1,{},[]]:path.endsWith('input_may_be_duplicated')?[undefined,null,1,'false',{},[]]:[undefined,1,'invalid',false];
   for(const value of values) {
    const e=evidence(kind);changeAt(e,path,(parent,key)=>parent[key]=value);
    assert.equal(A.verdict(kind,e).status,'inconclusive',`${kind}: ${path}=${String(value)}`);
   }
  }
 }
 for(const path of ['after.progress.SSRC','after.progress.Timestamp']) {
  const e=evidence();changeAt(e,path,(parent,key)=>parent[key]=0x100000000);assert.equal(A.verdict('move',e).status,'inconclusive');
 }
 const e=evidence();e.after.position.count=Number.MAX_SAFE_INTEGER;assert.equal(A.verdict('move',e).status,'inconclusive');
});
test('duplicate-window evidence is validated and cannot disguise frozen or regressed position',()=>{
 const replay=()=>{const e=evidence('kill');e.after.last_resume.duplicate_windows=[{SSRC:123,First:61,Last:80}];return e;};
 const e=replay();const got=A.verdict('kill',e);
 assert.equal(got.status,'pass');assert.equal(got.inputMayBeDuplicated,true);assert.deepEqual(got.duplicateWindows,[{SSRC:123,First:61,Last:80}]);
 for(const key of ['SSRC','First','Last']) {
  for(const value of [undefined,null,'1',NaN,Infinity,-1,1.5,Number.MAX_SAFE_INTEGER+1]) {
   const e=replay();if(value===undefined)delete e.after.last_resume.duplicate_windows[0][key];else e.after.last_resume.duplicate_windows[0][key]=value;
   assert.equal(A.verdict('kill',e).status,'inconclusive',`window ${key}=${String(value)}`);
  }
 }
 const reversed=replay();reversed.after.last_resume.duplicate_windows[0].First=81;assert.equal(A.verdict('kill',reversed).status,'inconclusive');
 const dishonest=replay();dishonest.after.last_resume.input_may_be_duplicated=false;assert.equal(A.verdict('kill',dishonest).status,'fail');
 const repeated=replay();repeated.observed_after.position=structuredClone(repeated.after.position);assert.equal(A.verdict('kill',repeated).status,'fail');
 const regressed=replay();regressed.after.position=p(6);regressed.observed_after.position=p(5);assert.equal(A.verdict('kill',regressed).status,'fail');
 // Known replay can repeat an input index while Consumed remains monotone.
 const duplicateIndex=replay();duplicateIndex.after.progress.Index=75;duplicateIndex.observed_after.progress.Index=70;assert.equal(A.verdict('kill',duplicateIndex).status,'pass');
 const noReplay=evidence('kill');noReplay.after.last_resume.input_may_be_duplicated=false;noReplay.after.last_resume.duplicate_windows=null;
 assert.equal(A.verdict('kill',noReplay).status,'pass');assert.equal(A.verdict('kill',noReplay).inputMayBeDuplicated,false);
});
