// aggregate_user_devices
//
// Source : ncgr.platform.devices     (one document per device registration)
// Target : ncgr.platform.users       (alias users_col, one document per user)
// Meta   : ncgr.platform.eventing_metadata
//
// Maintains a per-user registry of that user's devices, keyed by device id so a
// re-registration updates in place and duplicates are impossible.
//
// Ported from the original function, which read the pre-Sync-Gateway device
// schema. Field mapping:
//
//   doc.owner              -> doc.user_id
//   doc.device_identifier  -> doc.device_id
//   doc.phone_type         -> doc.metadata.model
//   doc.os_name            -> doc.metadata.os_name
//   doc.os_version         -> doc.metadata.os_version
//   doc.timestamp          -> doc.metadata.registered_at
//   doc.fcm_token          -> doc.push_token
//
// No write loop: the source collection is `devices` and the only write target is
// `users`, so this function cannot re-trigger itself.

function OnUpdate(doc, meta) {
    // Sync Gateway writes its own bookkeeping documents into this collection -
    // `_sync:syncInfo` is present right now. They are not device registrations.
    if (meta.id.startsWith("_sync") == true) return;

    // Without an identity the document cannot be aggregated. Indexing it under
    // "undefined" would quietly corrupt the registry for every such document, so
    // skip instead.
    if (!doc.user_id || !doc.device_id) return;

    var tenantId = doc.tenant_id || "${TENANT_ID}";
    var appName  = doc.app_name || "UnknownApp";

    // Key follows the convention used by every other collection in this bucket
    // (dev::, ntf::, evt::). Including the tenant keeps two tenants that share a
    // user_id from colliding on one aggregate.
    var userKey = "usr::" + tenantId + "::" + doc.user_id;

    var userDoc = users_col[userKey];
    if (!userDoc) {
        userDoc = { "tenant_id": tenantId, "user_id": doc.user_id, "apps": {} };
    }

    // Apps are nested under `apps` rather than hung off the document root. At the
    // root, an app named "user_id" or "tenant_id" would overwrite a real field.
    if (!userDoc.apps) {
        userDoc.apps = {};
    }
    if (!userDoc.apps[appName]) {
        userDoc.apps[appName] = { "devices": {} };
    }
    if (!userDoc.apps[appName].devices) {
        userDoc.apps[appName].devices = {};
    }

    // Strip out redundant data so the user doc doesn't get bloated: tenant, user
    // and app are already implied by their position in the structure.
    var md = doc.metadata || {};
    var deviceDetails = {
        "platform":      doc.platform,
        "model":         md.model,
        "os_name":       md.os_name,
        "os_version":    md.os_version,
        "registered_at": md.registered_at,
        "push_token":    doc.push_token,
        "status":        doc.status || "active"
    };

    // Keyed by device id, so a device that re-registers replaces its own entry.
    userDoc.apps[appName].devices[doc.device_id] = deviceDetails;
    userDoc.updated_at = new Date().toISOString();

    users_col[userKey] = userDoc;
}

// Removes a device from the registry when its registration is deleted.
//
// The original function had no OnDelete, which left dead devices in the
// aggregate. For a notification platform that means pushes addressed to a
// retired token. Delete this handler if you would rather keep the history.
//
// OnDelete receives only `meta` - the body is already gone - so the identity has
// to come from the key, which the app builds as dev::{tenant}::{user}::{device}.
// A key in any other shape is ignored rather than guessed at.
function OnDelete(meta, options) {
    if (meta.id.startsWith("_sync") == true) return;

    var parts = meta.id.split("::");
    if (parts.length != 4 || parts[0] != "dev") return;

    var tenantId = parts[1];
    var userId   = parts[2];
    var deviceId = parts[3];

    var userKey = "usr::" + tenantId + "::" + userId;
    var userDoc = users_col[userKey];
    if (!userDoc || !userDoc.apps) return;

    // app_name is not in the key, so look for the device across every app.
    var changed = false;
    for (var appName in userDoc.apps) {
        var devices = userDoc.apps[appName].devices;
        if (devices && devices[deviceId]) {
            delete devices[deviceId];
            changed = true;
        }
    }

    if (changed) {
        userDoc.updated_at = new Date().toISOString();
        users_col[userKey] = userDoc;
    }
}
