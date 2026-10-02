function (doc) {
  // Device registrations carry no channel. Import any document that looks like
  // a real registration; this skips Sync Gateway's own _sync:syncInfo, which
  // lives in this collection and has no user_id.
  return !!doc.user_id;
}
