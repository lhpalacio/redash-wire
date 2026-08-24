import SwiftUI

/// Hands the details to `redash-wire init`, so the binary stays the only thing
/// that writes the config file.
struct OnboardingView: View {
    @ObservedObject var model: AppModel
    @Environment(\.dismiss) private var dismiss

    @State private var redashURL = ""
    @State private var profileName = "default"
    @State private var apiKey = ""
    @State private var isWorking = false
    @State private var errorMessage: String?
    @State private var remedy: String?
    @State private var result: InitResult?

    private var canSubmit: Bool {
        !isWorking
            && !apiKey.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            && !redashURL.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            && !profileName.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            header

            if let result {
                success(result)
            } else {
                form
                if let errorMessage {
                    failure(errorMessage)
                }
                footer
            }
        }
        .padding(20)
        .frame(width: 460)
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Set up redash-wire")
                .font(.title2.weight(.semibold))
            Text("Connect to Redash so your database clients can query it.")
                .foregroundStyle(.secondary)
        }
    }

    private var form: some View {
        Grid(alignment: .leading, horizontalSpacing: 12, verticalSpacing: 10) {
            GridRow {
                Text("Redash URL")
                TextField("https://redash.example.com", text: $redashURL)
            }
            GridRow {
                Text("API key")
                SecureField("Your Redash user API key", text: $apiKey)
            }
            GridRow {
                Text("Profile")
                TextField("default", text: $profileName)
            }
        }
        .textFieldStyle(.roundedBorder)
        .disabled(isWorking)
    }

    private func failure(_ message: String) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(message)
                .foregroundStyle(.red)
            if let remedy {
                Text(remedy)
                    .foregroundStyle(.secondary)
                    .font(.callout)
            }
        }
        .fixedSize(horizontal: false, vertical: true)
    }

    private func success(_ result: InitResult) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Connected to Redash \(result.redashVersion)")
                .font(.headline)
            Text("Signed in as \(result.userName) (\(result.userEmail))")
                .foregroundStyle(.secondary)
            Text("\(result.dataSources) data source\(result.dataSources == 1 ? "" : "s") available")
                .foregroundStyle(.secondary)
            HStack {
                Spacer()
                Button("Done") { dismiss() }
                    .keyboardShortcut(.defaultAction)
            }
        }
    }

    private var footer: some View {
        HStack {
            if isWorking {
                ProgressView()
                    .controlSize(.small)
                Text("Testing the connection…")
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Button("Cancel") { dismiss() }
                .disabled(isWorking)
            Button("Test & Save") { submit() }
                .keyboardShortcut(.defaultAction)
                .disabled(!canSubmit)
        }
    }

    private func submit() {
        isWorking = true
        errorMessage = nil
        remedy = nil

        Task {
            defer { isWorking = false }
            do {
                result = try await model.runOnboarding(
                    redashURL: redashURL.trimmingCharacters(in: .whitespacesAndNewlines),
                    profile: profileName.trimmingCharacters(in: .whitespacesAndNewlines),
                    apiKey: apiKey.trimmingCharacters(in: .whitespacesAndNewlines)
                )
                // It lives in the config now.
                apiKey = ""
            } catch let error as WireError {
                errorMessage = error.message
                remedy = error.remedy
            } catch {
                errorMessage = error.localizedDescription
            }
        }
    }
}
