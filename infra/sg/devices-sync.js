function (doc, oldDoc, meta) {
  if (doc._deleted) {
    requireRole("admin");
    return;
  }
  if (!doc.user_id || !doc.tenant_id) {
    throw({forbidden: "device requires tenant_id and user_id"});
  }
  // Only the authenticated user may register a device in their own name.
  requireUser(doc.user_id);

  // Separate channel from notifications, so the app's notification replicator
  // does not pull device registrations it has no use for.
  var ch = "tenant::" + doc.tenant_id + "::user::" + doc.user_id + "::devices";
  channel(ch);
  access(doc.user_id, ch);
}
