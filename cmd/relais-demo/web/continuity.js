'use strict';
((root) => {
  const samples = (p) => p && Number.isSafeInteger(p.count) && p.count>0 && Number.isInteger(p.phase_samples) && p.phase_samples>=0 && p.phase_samples<24000 ? (p.count-1)*24000+p.phase_samples : null;
  const samePosition=(a,b)=>samples(a)!==null && samples(a)===samples(b);
  const sameProgress=(a,b)=>a && b && ['SSRC','Index','Timestamp','Consumed'].every((key)=>Number.isSafeInteger(a[key]) && a[key]===b[key]);
  function verdict(kind,evidence) {
    const result=(status,reasons,extra={})=>({status,reasons,...extra});
    if(!evidence || evidence.errors?.length) return result('inconclusive',evidence?.errors?.length?evidence.errors:['agent evidence missing']);
    const before=evidence.before,after=evidence.after,observed=evidence.observed_after,r=after?.last_resume,checkpoint=after?.checkpoint;
    if(samples(before?.position)===null || samples(after?.position)===null || samples(observed?.position)===null || !r || !checkpoint || typeof r.input_may_be_duplicated!=='boolean')return result('inconclusive',['agent position, resume, checkpoint or post-event observation missing']);
    const extras={positionBefore:before.position,positionRestored:r.position,positionAfter:observed.position,inputMayBeDuplicated:r.input_may_be_duplicated,duplicateWindows:r.duplicate_windows,checkpointAgeMs:r.checkpoint_age_ms,snapshotAgeMs:r.snapshot_age_ms};
    if(r.kind!==(kind==='kill'?'takeover':'planned_move') || r.number!==(before.last_resume?.number || 0)+1 || observed.last_resume?.number!==r.number)return result('fail',['resume does not belong to this event'],extras);
    if(!samePosition(r.position,checkpoint.position) || !sameProgress(r.progress,checkpoint.progress))return result('fail',['restore differs from the actual incoming snapshot'],extras);
    if(samples(after.position)<samples(r.position) || samples(observed.position)<=samples(r.position) || observed.progress?.Consumed<=r.progress?.Consumed)return result('fail',['agent did not continue after resume'],extras);
    if(kind==='kill') {
      const age=evidence.measured_checkpoint_age_ms,snapshotAge=evidence.measured_snapshot_age_ms;
      if(![age,snapshotAge,r.checkpoint_age_ms,r.snapshot_age_ms].every((x)=>Number.isFinite(x) && x>=0))return result('inconclusive',['measured checkpoint ages missing'],extras);
      if(Math.abs(age-r.checkpoint_age_ms)>0.001 || Math.abs(snapshotAge-r.snapshot_age_ms)>0.001 || snapshotAge<age)return result('fail',['resume age differs from the control-plane measurement'],extras);
      const rollbackMs=Math.max(0,(samples(before.position)-samples(r.position))/48);
      extras.rollbackMs=rollbackMs;
      // Snapshot age includes copy-to-commit time; write age alone can hide
      // storage latency. One 20 ms packet covers count quantization at capture.
      if(rollbackMs>snapshotAge+20)return result('fail',['rollback exceeds measured snapshot age plus one audio packet'],extras);
    } else {
      if(!evidence.exported)return result('inconclusive',['flushed source export missing'],extras);
      if(!samePosition(evidence.exported.position,r.position) || !sameProgress(evidence.exported.progress,r.progress))return result('fail',['planned move changed count, tone phase or consumed progress'],extras);
      if(r.input_may_be_duplicated)return result('fail',['planned move unexpectedly flagged duplicated input'],extras);
    }
    return result('pass',[],extras);
  }
  root.relaisContinuity={verdict,samples};
  if(typeof module==='object')module.exports=root.relaisContinuity;
})(typeof window==='object'?window:globalThis);
