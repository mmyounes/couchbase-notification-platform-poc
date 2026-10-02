import SwiftUI

struct ContentView: View {
    @EnvironmentObject var authManager: AuthAndSyncManager

    var body: some View {
        VStack(spacing: 20) {
            
            // Header
            HStack {
                Image(systemName: "server.rack")
                    .foregroundColor(.blue)
                Text("Notification Center")
                    .font(.title2)
                    .fontWeight(.bold)
            }
            .padding(.top)
            
            if authManager.isAuthenticated {
                
                // Status Card
                VStack(spacing: 8) {
                    Text("Couchbase Sync Status")
                        .font(.caption)
                        .foregroundColor(.gray)
                    
                    HStack {
                        Circle()
                            .fill(authManager.syncStatus.contains("Listening") ? Color.green : Color.orange)
                            .frame(width: 10, height: 10)
                        Text(authManager.syncStatus)
                            .font(.subheadline)
                            .bold()
                    }
                }
                .padding()
                .frame(maxWidth: .infinity)
                .background(Color(UIColor.secondarySystemBackground))
                .cornerRadius(12)
                
                Divider().padding(.vertical, 8)
                
                // Notifications List Header
                HStack {
                    Text("Recent Notifications")
                        .font(.headline)
                    Spacer()
                    // Unseen count alongside the total: the app marks everything seen
                    // once it is displayed, so this drops to 0 moments after a pull and
                    // makes the read receipt visible rather than invisible.
                    let unseen = authManager.notifications.filter { !$0.seen }.count
                    if unseen > 0 {
                        Text("\(unseen) new")
                            .font(.caption)
                            .padding(.horizontal, 8).padding(.vertical, 4)
                            .background(Color.red.opacity(0.15))
                            .foregroundColor(.red)
                            .clipShape(Capsule())
                    }
                    Text("\(authManager.notifications.count)")
                        .font(.caption)
                        .padding(6)
                        .background(Color.blue.opacity(0.1))
                        .foregroundColor(.blue)
                        .clipShape(Circle())
                }
                
                // The Animated List
                ScrollView {
                    LazyVStack(spacing: 12) {
                        if authManager.notifications.isEmpty {
                            Text("No new notifications.")
                                .foregroundColor(.gray)
                                .padding(.top, 40)
                        } else {
                            ForEach(authManager.notifications) { note in
                                HStack(alignment: .top, spacing: 15) {
                                    Image(systemName: note.seen ? "bell.fill" : "bell.badge.fill")
                                        .foregroundColor(note.seen ? .gray : .blue)
                                        .padding(.top, 2)

                                    VStack(alignment: .leading, spacing: 4) {
                                        if !note.subject.isEmpty {
                                            Text(note.subject)
                                                .font(.subheadline)
                                                .fontWeight(note.seen ? .regular : .semibold)
                                                .fixedSize(horizontal: false, vertical: true)
                                        }
                                        Text(note.message)
                                            .font(.body)
                                            .fixedSize(horizontal: false, vertical: true)

                                        HStack(spacing: 6) {
                                            Text(note.timestamp, style: .time)
                                            if !note.channel.isEmpty {
                                                Text("·"); Text(note.channel)
                                            }
                                            if !note.status.isEmpty {
                                                Text("·")
                                                Text(note.status)
                                                    .foregroundColor(note.status == "FAILED" ? .red : .gray)
                                            }
                                        }
                                        .font(.caption)
                                        .foregroundColor(.gray)
                                    }
                                    Spacer()
                                }
                                .padding()
                                .background(Color(UIColor.tertiarySystemBackground))
                                .cornerRadius(12)
                                .shadow(color: Color.black.opacity(0.05), radius: 3, x: 0, y: 2)
                                // This provides the nice native slide-in animation
                                .transition(.scale(scale: 0.95).combined(with: .opacity).combined(with: .move(edge: .top)))
                            }
                        }
                    }
                    .padding(.bottom)
                }
                
            } else {
                Spacer()
                Button(action: {
                    authManager.login()
                }) {
                    Text("Login with Keycloak")
                        .font(.headline)
                        .foregroundColor(.white)
                        .padding()
                        .frame(maxWidth: .infinity)
                        .background(Color.blue)
                        .cornerRadius(10)
                }
                Spacer()
            }
        }
        .padding(.horizontal)
        .background(Color(UIColor.systemGroupedBackground).edgesIgnoringSafeArea(.all))
    }
}
