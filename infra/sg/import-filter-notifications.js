function (doc) {
  // Only cblite notifications reach a device.
  //
  // One event fans out to several channels, and importing all of them meant a
  // single event arrived on the phone three times over, tagged email / sms /
  // push - one entry per provider the platform happened to use. Those three are
  // records of an outbound provider call, not things a user reads in the app.
  // cblite is the in-app channel, so exactly one document per event syncs.
  //
  // This also replaces the previous user allowlist. No user needs naming here
  // any more: send to anyone from the Send console and their cblite
  // notification syncs. And because the ~523M seeded documents are all
  // email / sms / push, none of them are imported.
  return doc.channel === "cblite";
}
