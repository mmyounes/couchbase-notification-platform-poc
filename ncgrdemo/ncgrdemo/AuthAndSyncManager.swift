//
//  AuthAndSyncManager.swift
//  ncgrdemo
//
//  Created by Mahmoud Younes on 10/04/2026.
//


import SwiftUI
import UIKit
import AuthenticationServices
import CouchbaseLiteSwift
import Combine
import CryptoKit
import Foundation

extension Data {
    // PKCE requires Base64URL encoding (no +, /, or padding =)
    func base64URLEncodedString() -> String {
        return self.base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }
}

/// One notification as the UI needs it.
///
/// Field names mirror the server document exactly (spec section 5.2) so there is one
/// vocabulary across the pipeline, the web console and this app.
struct AppNotification: Identifiable, Equatable {
    let id: String
    let subject: String
    let message: String
    let timestamp: Date
    let type: String
    let channel: String
    let status: String
    let seen: Bool
}

/// Keeps the device's `seen` flag when the server rewrites a notification.
///
/// The pipeline mutates these documents server-side after every delivery attempt
/// (`delivery.trials`, `status`, `delivery.last_error`), while the device only ever
/// owns `seen`. Without a resolver, Couchbase Lite's default "most recent revision
/// wins" would let a tracking update silently clear a read receipt, or let the device
/// push a stale body back over fresher delivery state.
///
/// Taking the remote document as the base and re-applying only `seen` gives each side
/// exactly the fields it owns. This is the client half; the server half is a Sync
/// Gateway sync function rejecting device writes to anything but `seen`, which belongs
/// to the Sync Gateway stage of the work.
final class SeenPreservingConflictResolver: ConflictResolverProtocol {
    // `Document` must be module-qualified: the iOS 27 SDK added SwiftUI.Document,
    // and this file imports both SwiftUI and CouchbaseLiteSwift, so the bare name
    // is ambiguous and fails to compile. (`Conflict` needs no qualifier - only
    // CouchbaseLiteSwift declares it.)
    func resolve(conflict: Conflict) -> CouchbaseLiteSwift.Document? {
        // Deleted on the server: honour the deletion.
        guard let remote = conflict.remoteDocument else { return nil }
        guard let local = conflict.localDocument else { return remote }

        let merged = remote.toMutable()
        // `seen` is one-way: false -> true. A local true always wins, a local false
        // never overwrites a server true (another device may have read it).
        if local.boolean(forKey: "seen") || remote.boolean(forKey: "seen") {
            merged.setBoolean(true, forKey: "seen")
        }
        return merged
    }
}

class AuthAndSyncManager: NSObject, ObservableObject, ASWebAuthenticationPresentationContextProviding {

    var database: Database?

    // Notification replicator
    var replicator: Replicator?

    // UI State
    @Published var isAuthenticated = false
    @Published var syncStatus = "Stopped"

    @Published var notifications: [AppNotification] = []
    var liveQueryToken: ListenerToken? // Retain the query listener!

    // ---------------------------------------------------------------------
    // Server identifiers. Three values below default to a name containing
    // "ncgr" but they are NOT the same thing and are not interchangeable:
    //
    //   tenantId  - the Couchbase tenant (server TENANT_ID)
    //   appName   - which app produced a notification; demo data has six
    //   clientId  - the OAuth2 client registered in Keycloak
    //
    // Change each to match the corresponding server-side value.
    // ---------------------------------------------------------------------

    /// Written into this device's registration as `app_name`, and carried on the
    /// notifications it produces. Purely a label for grouping - the seeded demo
    /// data uses six of them (ncgrdemo, portal, hr_self_service, procurement,
    /// payroll, licensing). Unrelated to `clientId` despite the shared name.
    let appName = "ncgrdemo"

    /// Must match the server's `TENANT_ID`. It is part of the sync channel name and
    /// of every document key, so a mismatch yields a silently empty inbox.
    let tenantId = "ncgr"

    /// The scope holding `notifications` and `devices` on the server. Couchbase Lite
    /// collections are matched to Sync Gateway by scope AND name, so this is not
    /// cosmetic - the wrong scope replicates nothing.
    let scopeName = "platform"

    /// Keycloak realm holding the users and the client below.
    let realm = "myrealm"

    /// The OAuth2/OIDC **client** registered in Keycloak - this app's identity to
    /// the identity provider, nothing to do with the Couchbase tenant. It is sent
    /// as `client_id` on both the authorization and token requests, and must equal
    /// `oidc.providers.keycloak.client_id` in Sync Gateway's db-config.json. A
    /// mismatch fails the login with "invalid client", not with a sync error.
    let clientId = "ncgrdemo"

    /// Must be registered as a valid redirect URI on that Keycloak client, and the
    /// scheme ("ncgrdemo") must be declared in Info.plist under CFBundleURLSchemes.
    let redirectURI = "ncgrdemo://auth"

    // Must be the address Keycloak itself advertises (KC_HOSTNAME), not just any
    // address that reaches it. The `iss` claim is minted from that setting, and
    // Sync Gateway rejects a token whose issuer does not match its own config
    // byte for byte - so authenticating via a different host silently fails.
    //
    // This is a DuckDNS name rather than an IP on purpose. The hosts have no
    // Elastic IPs, so every restart used to change this value, and changing it
    // means changing the issuer in Sync Gateway too - a config PUT that restarts
    // the ~524M document import scan. The name is repointed at the new address by
    // duckdns@<prefix>-kc.timer on the Keycloak host, so the issuer never changes and
    // neither does this constant.
    //
    // The port is part of the issuer string. Moving Keycloak off 8080 is another
    // full re-import, so do not "tidy" it to port 80.
    // CHANGE ME: your Keycloak address, exactly as KC_HOSTNAME advertises it.
    //
    // Changing this means changing Info.plist too: the host must appear under
    // NSAppTransportSecurity > NSExceptionDomains or iOS refuses the cleartext
    // request at runtime with NSURLErrorDomain -1022, naming ATS rather than the
    // mismatch. The app builds and launches either way.
    let keycloakDomain = "http://CHANGEME-kc.duckdns.org:8080"

    // CHANGE ME: your Sync Gateway address. Also needs an Info.plist ATS
    // exception, as above - the replicator's ws:// is blocked by the same policy.
    let syncGateway = "CHANGEME-sg.duckdns.org:4984"

    /// APNs / FCM registration token, set by the push-registration callback before
    /// `pushDeviceInfo` runs. Left nil until then - a hardcoded token registers every
    /// install as the same device and sends that user's pushes to someone else's phone.
    var pushToken: String?

    /// The server writes millisecond precision (`2026-08-29T08:34:19.752Z`) and the
    /// spec makes that a correctness requirement, not a formatting one: the value must
    /// equal the time embedded in the event ULID. `ISO8601DateFormatter` ignores
    /// fractional seconds unless told, and returns nil rather than truncating - which
    /// would silently date every notification to "now".
    private static let isoWithMillis: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()

    private static let isoPlain: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }()

    private static func parseTimestamp(_ s: String) -> Date? {
        isoWithMillis.date(from: s) ?? isoPlain.date(from: s)
    }

    // Store the verifier temporarily during the login flow
    private var currentCodeVerifier: String = ""

    // 1. Helper functions to generate PKCE strings
    private func generateCodeVerifier() -> String {
        var buffer = [UInt8](repeating: 0, count: 32)
        _ = SecRandomCopyBytes(kSecRandomDefault, buffer.count, &buffer)
        return Data(buffer).base64URLEncodedString()
    }

    private func generateCodeChallenge(verifier: String) -> String {
        guard let data = verifier.data(using: .ascii) else { return "" }
        let hash = SHA256.hash(data: data)
        return Data(hash).base64URLEncodedString()
    }

    /// Set to true only when debugging a broken checkpoint. Wiping the local database
    /// on every launch defeats the point of an offline-first store and forces a full
    /// re-pull of the user's history each time.
    static let resetLocalDatabaseOnLaunch = false

    override init() {
        super.init()

        if Self.resetLocalDatabaseOnLaunch {
            // Separate do/catch: `Database.delete` throws when the database does not
            // exist, which is the normal case on a fresh install. Sharing a catch with
            // the open below meant the very first launch threw here, skipped the open
            // entirely, and left `database` nil - after which every guard in this class
            // returned silently and the app simply never synced.
            do {
                try Database.delete(withName: "db")
                print("Local database deleted.")
            } catch {
                print("No existing database to delete: \(error)")
            }
        }

        do {
            // Create a new or open the existing cblite database
            database = try Database(name: "db") // for simplicity, keep the name similar to the sync_gateway database name
        } catch {
            print("Error initializing database: \(error)")
        }
    }

    // IDM login using Auth Code Flow
    func login() {
        self.currentCodeVerifier = generateCodeVerifier()
        let codeChallenge = generateCodeChallenge(verifier: currentCodeVerifier)

        // Safely construct the URL
        var components = URLComponents(string: "\(keycloakDomain)/realms/\(realm)/protocol/openid-connect/auth")!
        components.queryItems = [
            URLQueryItem(name: "client_id", value: clientId),
            URLQueryItem(name: "response_type", value: "code"),
            URLQueryItem(name: "redirect_uri", value: redirectURI),
            URLQueryItem(name: "scope", value: "openid profile email"),
            URLQueryItem(name: "code_challenge", value: codeChallenge),
            URLQueryItem(name: "code_challenge_method", value: "S256")
        ]

        guard let authURL = components.url else {
            print("Failed to construct Auth URL")
            return
        }

        let scheme = "ncgrdemo"

        let session = ASWebAuthenticationSession(url: authURL, callbackURLScheme: scheme) { callbackURL, error in
            guard error == nil, let callbackURL = callbackURL else {
                print("Authentication Session Error: \(String(describing: error))")
                return
            }

            guard let components = URLComponents(url: callbackURL, resolvingAgainstBaseURL: false),
                  let codeItem = components.queryItems?.first(where: { $0.name == "code" }),
                  let code = codeItem.value else {
                print("Could not extract authorization code from callback URL")
                return
            }

            self.exchangeCodeForToken(authCode: code)
        }

        session.presentationContextProvider = self
        session.start()
    }

    func exchangeCodeForToken(authCode: String) {
        let tokenURL = URL(string: "\(keycloakDomain)/realms/\(realm)/protocol/openid-connect/token")!
        var request = URLRequest(url: tokenURL)
        request.httpMethod = "POST"
        request.setValue("application/x-www-form-urlencoded", forHTTPHeaderField: "Content-Type")

        // Safely URL-Encode the POST body using URLComponents
        var urlComponents = URLComponents()
        urlComponents.queryItems = [
            URLQueryItem(name: "grant_type", value: "authorization_code"),
            URLQueryItem(name: "client_id", value: clientId),
            URLQueryItem(name: "code", value: authCode),
            URLQueryItem(name: "redirect_uri", value: redirectURI),
            URLQueryItem(name: "code_verifier", value: self.currentCodeVerifier)
        ]

        // The .query property automatically URL-encodes everything perfectly
        // We replace "+" with "%2B" just in case Keycloak is picky about space encoding
        let bodyString = urlComponents.query?.replacingOccurrences(of: "+", with: "%2B") ?? ""
        request.httpBody = bodyString.data(using: .utf8)

        URLSession.shared.dataTask(with: request) { data, response, error in
            // 1. Catch actual network errors (like ATS blocks)
            if let error = error {
                print("Network Error: \(error.localizedDescription)")
                return
            }

            // 2. Catch HTTP Status Code errors
            if let httpResponse = response as? HTTPURLResponse {
                print("Keycloak HTTP Status: \(httpResponse.statusCode)")
            }

            guard let data = data else { return }

            // 3. Print the RAW server response so you can see EXACTLY what Keycloak thinks
            if let rawResponse = String(data: data, encoding: .utf8) {
                print("Raw Keycloak Response: \(rawResponse)")
            }

            do {
                if let json = try JSONSerialization.jsonObject(with: data, options: []) as? [String: Any] {

                    // Did Keycloak explicitly hand us an error?
                    if let errorMsg = json["error_description"] as? String {
                         print("Keycloak Error: \(errorMsg)")
                         return
                    }

                    if let idToken = json["id_token"] as? String {
                        DispatchQueue.main.async {
                            print("Token Exchange Successful!")
                            self.isAuthenticated = true

                            // Start the Couchbase sync processes
                            if let username = self.extractUsername(from: idToken) {
                                self.pushDeviceInfo(username: username, idToken: idToken)
                                self.startNotificationListener(username: username, idToken: idToken)
                            } else {
                                print("Could not extract username from token")
                            }
                        }
                    } else {
                        print("id_token was missing from a successful response!")
                    }
                }
            } catch {
                print("JSON parsing error: \(error.localizedDescription)")
            }

            // Clear the verifier from memory for security once done
            self.currentCodeVerifier = ""

        }.resume()
    }

    func pushDeviceInfo(username: String, idToken: String) {
        do {
            guard let devicesColl = try database?.createCollection(name: "devices", scope: scopeName) else { return }

            // Gather Device Info
            let device = UIDevice.current
            let deviceId = device.identifierForVendor?.uuidString ?? "unknown_id"

            // Key shape per spec section 5: dev::{tenant}::{user_id}::{device_id}.
            // Deterministic, so a reinstall updates the row rather than orphaning it.
            let doc = MutableDocument(id: "dev::\(tenantId)::\(username)::\(deviceId)")
            doc.setString(tenantId, forKey: "tenant_id")
            doc.setString(username, forKey: "user_id")
            doc.setString(deviceId, forKey: "device_id")
            doc.setString("ios", forKey: "platform")
            doc.setString("active", forKey: "status")
            doc.setString(appName, forKey: "app_name")
            if let pushToken {
                doc.setString(pushToken, forKey: "push_token")
            }

            // Everything device-specific lives under `metadata` so the top level stays
            // the stable contract the requirements name.
            let metadata = MutableDictionaryObject()
            metadata.setString(device.model, forKey: "model")
            metadata.setString(device.systemName, forKey: "os_name")
            metadata.setString(device.systemVersion, forKey: "os_version")
            metadata.setDate(Date(), forKey: "registered_at")
            doc.setDictionary(metadata, forKey: "metadata")

            try devicesColl.save(document: doc)
            print("Device info document saved locally: \(doc.id)")

            // Configure the Push-Only Replicator
            let targetURL = URL(string: "ws://\(syncGateway)/db")!
            let target = URLEndpoint(url: targetURL)

            let colConfig = CollectionConfiguration(collection: devicesColl)

            var config = ReplicatorConfiguration(collections: [colConfig], target: target)

            // IMPORTANT: Set to Push only, and Continuous to false
            config.replicatorType = .push
            config.continuous = false
            config.headers = ["Authorization": "Bearer \(idToken)"]

            // We use a local replicator instance for this one-shot task
            // so it doesn't interfere with your main continuous replicator
            let telemetryReplicator = Replicator(config: config)

            // Add Document-level listener to catch 409 conflicts
            let deviceInfoDocumentToken = telemetryReplicator.addDocumentReplicationListener { replication in
                let direction = replication.isPush ? "PUSH" : "PULL"

                for document in replication.documents {
                    if let error = document.error as NSError? {
                        if error.code == 409 || error.code == 10409 {
                            print("[409 CONFLICT] Direction: \(direction), Doc ID: \(document.id)")
                        } else {
                            print("[SYNC ERROR] Doc ID: \(document.id), Error: \(error.localizedDescription)")
                        }
                    } else {
                        print("Successfully \(direction)ed device doc: \(document.id)")
                    }
                }
            }
            _ = deviceInfoDocumentToken

            // Listen for when the device info push finishes
            let token = telemetryReplicator.addChangeListener { change in
                let status = change.status
                print("Telemetry Status is: \(status.activity)")

                if status.activity == .stopped {
                    if let error = status.error {
                        print("Telemetry Sync Error: \(error.localizedDescription)")
                    } else {
                        print("Telemetry Sync Complete!")
                    }
                }
            }
            _ = token

            // Retained for the lifetime of the push; a local `let` would be released
            // as soon as this function returns and the push would never finish.
            self.deviceReplicator = telemetryReplicator
            telemetryReplicator.start()

        } catch {
            print("Error saving device info locally: \(error)")
            return
        }
    }

    /// Held so the one-shot device push is not deallocated mid-flight.
    private var deviceReplicator: Replicator?

    func startNotificationListener(username: String, idToken: String) {

        do {
            // Ensure the local collection exists
            guard let notificationsColl = try database?.createCollection(name: "notifications", scope: scopeName) else { return }

            // Configure the collection to pull from this app's specific channel.
            //
            // This must match `sync_channels` on the server document exactly. The
            // pipeline writes one channel per user, built by `syncChannel()` in
            // util.go as "tenant::{tenant}::user::{user_id}".
            var colConfig = CollectionConfiguration(collection: notificationsColl)
            let expectedChannel = "tenant::\(tenantId)::user::\(username)"
            print("Subscribing to channel: \(expectedChannel)")
            // IMPORTANT to specify the expected channels this app will listen to. Otherwise, it will listen to all notifications sent to this user
            colConfig.channels = [expectedChannel]

            // Keep the device's read receipt when the server rewrites the document.
            colConfig.conflictResolver = SeenPreservingConflictResolver()

            // Set up the Replicator
            let targetURL = URL(string: "ws://\(syncGateway)/db")!
            let target = URLEndpoint(url: targetURL)

            var config = ReplicatorConfiguration(collections: [colConfig] ,target: target)

            // pushAndPull, not pull: notifications come down, and `seen` goes back up.
            // A pull-only replicator would mark documents read locally and never tell
            // anyone.
            config.replicatorType = .pushAndPull
            config.continuous = true
            config.headers = ["Authorization": "Bearer \(idToken)"]

            // Retain and start the replicator
            self.replicator = Replicator(config: config)

            // Listen for document-level sync events
            let documentReplicationToken = self.replicator?.addDocumentReplicationListener { replication in

                let direction = replication.isPush ? "PUSH" : "PULL"

                // 2. Loop through the batch of documents that just attempted to sync
                for document in replication.documents {

                    // 3. Check if this specific document encountered an error
                    if let error = document.error as NSError? {

                        // Couchbase maps standard HTTP 409 to its internal 10409 code
                        if error.code == 409 || error.code == 10409 {
                            print("[409 CONFLICT DETECTED]")
                            print("   Direction: \(direction)")
                            print("   Document ID: \(document.id)")
                            print("   Error Msg: \(error.localizedDescription)")
                        } else {
                            // Catch other document-level errors (like 403 Forbidden)
                            print("[SYNC ERROR]")
                            print("   Direction: \(direction)")
                            print("   Document ID: \(document.id)")
                            print("   Error Code: \(error.code)")
                            print("   Error Msg: \(error.localizedDescription)")
                        }

                    } else {
                        // If there's no error, the document synced successfully
                        print("Successfully \(direction)ed doc: \(document.id)")
                    }
                }
            }
            _ = documentReplicationToken

            // Replicator status that shows in the UI
            self.replicator?.addChangeListener { [weak self] change in
                DispatchQueue.main.async {
                    if let error = change.status.error {
                        self?.syncStatus = "Error: \(error.localizedDescription)"
                    } else {
                        switch change.status.activity {
                        case .busy: self?.syncStatus = "Syncing Notifications..."
                        case .idle: self?.syncStatus = "Listening (Up to date)"
                        case .offline: self?.syncStatus = "Offline"
                        case .stopped: self?.syncStatus = "Stopped"
                        case .connecting: self?.syncStatus = "Connecting..."
                        @unknown default: self?.syncStatus = "Unknown"
                        }
                    }
                }
            }

            // No reset: the checkpoint is what makes a restart cheap. Resetting forces
            // a full re-pull of the user's entire history on every login.
            self.replicator?.start()

            // 5. Set up a Live Query to react to new notifications instantly
            self.observeNewNotifications(in: notificationsColl)

        } catch {
            print("Error setting up notification listener: \(error)")
        }
    }

    private func observeNewNotifications(in collection: Collection) {
        // Field names are the server's (spec section 5.2). There is deliberately no
        // `type` filter: collections provide type discrimination, so these documents
        // carry no discriminator field - `type` holds the business event type
        // ("document_expiry", "reset_password"), and filtering on a fixed value here
        // matched nothing at all.
        let query = QueryBuilder
            .select(SelectResult.expression(Meta.id),
                    SelectResult.property("subject"),
                    SelectResult.property("message"),
                    SelectResult.property("timestamp"),
                    SelectResult.property("type"),
                    SelectResult.property("channel"),
                    SelectResult.property("status"),
                    SelectResult.property("seen"))
            .from(DataSource.collection(collection))
            .orderBy(Ordering.property("timestamp").descending())

        self.liveQueryToken = query.addChangeListener { [weak self] change in
            guard let self = self, let results = change.results else { return }
            var newNotifications: [AppNotification] = []

            for result in results {
                let id = result.string(at: 0) ?? UUID().uuidString
                let subject = result.string(at: 1) ?? ""
                let message = result.string(at: 2) ?? "You have a new notification"
                let dateStr = result.string(at: 3) ?? ""
                let type = result.string(at: 4) ?? ""
                let channel = result.string(at: 5) ?? ""
                let status = result.string(at: 6) ?? ""
                let seen = result.boolean(at: 7)

                newNotifications.append(AppNotification(
                    id: id, subject: subject, message: message,
                    timestamp: Self.parseTimestamp(dateStr) ?? Date(),
                    type: type, channel: channel, status: status, seen: seen))
            }

            // Hop to the main thread and animate the UI update
            DispatchQueue.main.async {
                withAnimation(.spring(response: 0.4, dampingFraction: 0.8)) {
                    self.notifications = newNotifications
                }
            }

            // Everything just pulled is now on screen, so acknowledge it. Marking runs
            // off the main thread because it writes to the database.
            let unseen = newNotifications.filter { !$0.seen }.map(\.id)
            if !unseen.isEmpty {
                self.markAsSeen(ids: unseen, in: collection)
            }
        }
    }

    /// Sets `seen = true` locally; the continuous pushAndPull replicator carries it to
    /// Sync Gateway.
    ///
    /// This re-triggers the live query, which is intended and terminates: the second
    /// pass finds nothing unseen and writes nothing. Only `seen` is touched, so a
    /// pushed revision differs from the server's by exactly that field.
    private func markAsSeen(ids: [String], in collection: Collection) {
        DispatchQueue.global(qos: .utility).async {
            var marked = 0
            for id in ids {
                do {
                    guard let doc = try collection.document(id: id) else { continue }
                    if doc.boolean(forKey: "seen") { continue }   // already acknowledged
                    let mutable = doc.toMutable()
                    mutable.setBoolean(true, forKey: "seen")
                    try collection.save(document: mutable)
                    marked += 1
                } catch {
                    print("Could not mark \(id) as seen: \(error)")
                }
            }
            if marked > 0 {
                print("Marked \(marked) notification(s) as seen; replicator will push them.")
            }
        }
    }

    func extractUsername(from jwtToken: String) -> String? {
        let segments = jwtToken.components(separatedBy: ".")
        guard segments.count > 1 else { return nil }

        var base64String = segments[1]

        // 1. Convert Base64URL to standard Base64
        base64String = base64String.replacingOccurrences(of: "-", with: "+")
        base64String = base64String.replacingOccurrences(of: "_", with: "/")

        // 2. Pad the string with "=" so its length is a multiple of 4
        let requiredLength = Int(4 * ceil(Double(base64String.count) / 4.0))
        let paddingLength = requiredLength - base64String.count
        if paddingLength > 0 {
            base64String += String(repeating: "=", count: paddingLength)
        }

        // 3. Decode the Data and parse the JSON
        guard let data = Data(base64Encoded: base64String, options: .ignoreUnknownCharacters),
              let json = try? JSONSerialization.jsonObject(with: data, options: []) as? [String: Any] else {
            return nil
        }

        // 4. Return the preferred_username (or fallback to the subject UUID)
        return json["preferred_username"] as? String ?? json["sub"] as? String
    }

    func presentationAnchor(for session: ASWebAuthenticationSession) -> ASPresentationAnchor {
        let scenes = UIApplication.shared.connectedScenes
        let windowScene = scenes.first as? UIWindowScene
        return windowScene?.windows.first { $0.isKeyWindow } ?? ASPresentationAnchor(windowScene: windowScene!)
    }
}
