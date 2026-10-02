//
//  ncgrdemoApp.swift
//  ncgrdemo
//
//  Created by Mahmoud Younes on 04/12/2025.
//

import SwiftUI

@main
struct ncgrdemoApp: App {
    @StateObject private var authManager = AuthAndSyncManager()

    var body: some Scene {
        WindowGroup {
            ContentView()
                .environmentObject(authManager)
        }
    }
}
