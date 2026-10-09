import SwiftUI

/// Hands the details to `redash-wire init`, so the binary stays the only thing
/// that writes the config file.
struct OnboardingView: View {
    @ObservedObject var model: AppModel

    @State private var redashURL = ""
    @State private var profileName = "default"
    @State private var apiKey = ""
    @State private var readOnly = false
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
        .padding(16)
        .glassPanel(cornerRadius: 16)
        .padding(10)
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

    /// The labels are the fields' own, so VoiceOver reads "Redash URL" rather
    /// than the placeholder.
    private var form: some View {
        Form {
            TextField("Redash URL", text: $redashURL, prompt: Text("https://redash.example.com"))
            SecureField("API key", text: $apiKey, prompt: Text("Your Redash user API key"))
            if let profilePage {
                Link("Find your API key in Redash", destination: profilePage)
                    .font(.callout)
            }
            TextField("Profile", text: $profileName, prompt: Text("default"))
            Toggle("Read-only: refuse writes and schema changes", isOn: $readOnly)
                .toggleStyle(.checkbox)
        }
        .textFieldStyle(.roundedBorder)
        .disabled(isWorking)
    }

    /// Redash shows your API key on your own profile page.
    private var profilePage: URL? {
        guard let base = URL(string: normalizedURL), base.host != nil else { return nil }
        return base.appendingPathComponent("users/me")
    }

    /// A bare host is the usual thing to paste.
    private var normalizedURL: String {
        let trimmed = redashURL.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty, !trimmed.contains("://") else { return trimmed }
        return "https://" + trimmed
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
            if result.readOnly {
                Text("Read-only: writes are refused")
                    .foregroundStyle(.secondary)
            }
            HStack {
                Spacer()
                Button("Done") { WindowPresenter.shared.close("onboarding") }
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
            Button("Cancel") { WindowPresenter.shared.close("onboarding") }
                .keyboardShortcut(.cancelAction)
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
                    redashURL: normalizedURL,
                    profile: profileName.trimmingCharacters(in: .whitespacesAndNewlines),
                    apiKey: apiKey.trimmingCharacters(in: .whitespacesAndNewlines),
                    readOnly: readOnly
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
