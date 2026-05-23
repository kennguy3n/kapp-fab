//! End-to-end example: SSO sign-in, then list KTypes.
//!
//! Run against a kapp-fab gRPC server. Pass the endpoint, the
//! KChat OAuth code, and the redirect URI as env vars:
//!
//! ```bash
//! KAPP_ENDPOINT=https://api.example.com \
//! KAPP_SSO_CODE=<oauth-code> \
//! KAPP_REDIRECT_URI=https://app.example.com/cb \
//!   cargo run --example sso_then_list_ktypes
//! ```
//!
//! `KAPP_ENDPOINT` defaults to `http://127.0.0.1:9090` if unset
//! (matches the local-dev `KAPP_GRPC_ADDR` from the monorepo's
//! `services/api`).

use std::env;
use std::process::ExitCode;

use kapp_sdk::{ClientConfig, KappClient, KappError};

#[tokio::main]
async fn main() -> ExitCode {
    // Initialise structured logging so the SDK's tracing spans
    // appear in stderr. Useful when debugging the auth flow.
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("kapp_sdk=info")),
        )
        .with_writer(std::io::stderr)
        .init();

    let endpoint =
        env::var("KAPP_ENDPOINT").unwrap_or_else(|_| "http://127.0.0.1:9090".to_string());
    let Ok(code) = env::var("KAPP_SSO_CODE") else {
        eprintln!(
            "error: KAPP_SSO_CODE is required; set it to the KChat OAuth code returned to your redirect_uri"
        );
        return ExitCode::from(2);
    };
    let redirect_uri =
        env::var("KAPP_REDIRECT_URI").unwrap_or_else(|_| "https://app.example.com/cb".to_string());

    match run(&endpoint, &code, &redirect_uri).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(err) => {
            eprintln!("error: {err}");
            // Walk the cause chain so the operator sees the
            // underlying transport / status / refresh failure.
            let mut source = err.source();
            while let Some(s) = source {
                eprintln!("  caused by: {s}");
                source = s.source();
            }
            ExitCode::FAILURE
        }
    }
}

async fn run(endpoint: &str, code: &str, redirect_uri: &str) -> Result<(), KappError> {
    let cfg = ClientConfig::builder(endpoint).build()?;
    let client = KappClient::connect(cfg).await?;

    println!("→ exchanging SSO code at {endpoint}");
    let result = client.auth().exchange(code, redirect_uri, None).await?;
    // The proto contract allows `tenants` to be empty (e.g. when the
    // SSO user has no membership rows). Fall back to "(none)" so the
    // example never panics; production callers should check
    // `result.tenants` explicitly and surface the empty case to UX.
    let tenant_name = result.tenants.first().map_or("(none)", |t| t.name.as_str());
    println!(
        "  signed in as {} ({}) on tenant {} ({})",
        result.user.display_name, result.user.email, tenant_name, result.tenant_id
    );
    println!("  access token expires in {}s", result.expires_in);

    println!("→ listing KTypes");
    let ktypes = client.ktype().list().await?;
    if ktypes.is_empty() {
        println!("  (registry is empty)");
    } else {
        for kt in &ktypes {
            println!(
                "  • {} v{} (created at {})",
                kt.name, kt.version, kt.created_at
            );
        }
    }
    Ok(())
}

// Pull in `Error` so we can walk the cause chain in main().
use std::error::Error as _;
