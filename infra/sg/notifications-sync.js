function (doc, oldDoc, meta) {
  // Devices may not delete notifications. requireRole throws for an authenticated
  // device user and is a no-op during import, so this constrains devices only.
  if (doc._deleted) {
    requireRole("admin");
    return;
  }

  // Only the in-app channel reaches a device. The import filter already keeps
  // email/sms/push out, but channel membership is decided HERE, so this is the
  // enforcing copy: a provider notification that somehow got imported (an
  // on-demand admin GET, or an import predating this rule) is assigned no
  // channel and is therefore visible to nobody. Returning without calling
  // channel() is also what lets _resync retire documents imported earlier.
  if (doc.channel !== "cblite") {
    return;
  }

  // Route by the channel array the pipeline already writes (util.go syncChannel:
  // "tenant::{tenant}::user::{user_id}"). Deriving a channel name here instead
  // would duplicate that rule in a second place and let the two drift.
  if (!doc.sync_channels || !doc.user_id) {
    throw({forbidden: "notification requires sync_channels and user_id"});
  }
  channel(doc.sync_channels);
  access(doc.user_id, doc.sync_channels);

  // A device owns exactly one field: seen. Everything else belongs to the
  // pipeline, which rewrites these documents after every delivery attempt.
  // Compare field by field rather than stringifying the whole body, so top-level
  // key ordering cannot produce a false rejection.
  if (oldDoc && !oldDoc._deleted) {
    var k;
    for (k in doc) {
      if (k.charAt(0) === '_' || k === 'seen') continue;
      if (JSON.stringify(doc[k]) !== JSON.stringify(oldDoc[k])) { requireRole("admin"); return; }
    }
    for (k in oldDoc) {
      if (k.charAt(0) === '_' || k === 'seen') continue;
      if (!(k in doc)) { requireRole("admin"); return; }
    }
  }
}
