//! Demonstrates the SDK's client-side JSON Schema validation on
//! the KType `Register` path.
//!
//! Run after running `sso_then_list_ktypes`, since this example
//! reuses the same env vars and assumes you have credentials.
//!
//! ```bash
//! KAPP_ENDPOINT=https://api.example.com \
//! KAPP_SSO_CODE=<oauth-code> \
//! KAPP_REDIRECT_URI=https://app.example.com/cb \
//!   cargo run --example register_ktype_with_validation
//! ```
//!
//! The example registers two KTypes:
//! 1. A valid schema — succeeds via the real RPC.
//! 2. A schema with `"type": 12345` — fails client-side **without
//!    issuing the RPC**, demonstrating the SDK's typed
//!    `SchemaInvalid` error path.

use std::env;
use std::error::Error as _;
use std::process::ExitCode;

use kapp_sdk::{ClientConfig, KappClient, KappError};
use serde_json::json;

#[tokio::main]
async fn main() -> ExitCode {
    let endpoint =
        env::var("KAPP_ENDPOINT").unwrap_or_else(|_| "http://127.0.0.1:9090".to_string());
    let Ok(code) = env::var("KAPP_SSO_CODE") else {
        eprintln!("error: KAPP_SSO_CODE is required");
        return ExitCode::from(2);
    };
    let redirect_uri =
        env::var("KAPP_REDIRECT_URI").unwrap_or_else(|_| "https://app.example.com/cb".to_string());

    match run(&endpoint, &code, &redirect_uri).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(err) => {
            eprintln!("error: {err}");
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
    client.auth().exchange(code, redirect_uri, None).await?;

    // 1. Valid schema — gets registered.
    let valid = json!({
        "$schema": "http://json-schema.org/draft-07/schema#",
        "type": "object",
        "required": ["sku", "qty"],
        "properties": {
            "sku": { "type": "string", "minLength": 1 },
            "qty": { "type": "integer", "minimum": 0 }
        }
    });
    let reg = client.ktype().register("inventory_item", 1, valid).await?;
    println!("registered {} v{}", reg.name, reg.version);

    // 2. Invalid schema — caught locally, no RPC issued.
    let invalid = json!({ "type": 12345 });
    let err = client
        .ktype()
        .register("inventory_item_broken", 1, invalid)
        .await;
    match err {
        Err(KappError::SchemaInvalid { errors }) => {
            println!("invalid schema rejected locally:");
            for e in errors {
                println!("  • {e}");
            }
        }
        Err(other) => {
            return Err(other);
        }
        Ok(_) => {
            println!("(unexpected) invalid schema was accepted by the SDK");
        }
    }
    Ok(())
}
