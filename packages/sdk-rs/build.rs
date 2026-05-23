//! Build script that compiles the kapp-fab v1 protobuf surface
//! into Rust client bindings via `tonic-build` + `protox`.
//!
//! Why two crates: `protox` is a pure-Rust `.proto` compiler that
//! emits a `FileDescriptorSet` without requiring a system `protoc`
//! binary. Feeding that descriptor set into `tonic-build` keeps the
//! Rust SDK buildable in environments where downstream consumers
//! (e.g. a `cargo install kapp-sdk` user, or a CI runner that does
//! not pre-install `protoc`) lack the protobuf toolchain. The Go
//! side of the monorepo still uses real `protoc` via `buf`; this is
//! the Rust-specific equivalent, sharing the same `.proto` sources.

use std::env;
use std::error::Error;
use std::path::PathBuf;

fn main() -> Result<(), Box<dyn Error>> {
    let manifest_dir = PathBuf::from(env::var("CARGO_MANIFEST_DIR")?);
    let workspace_root = manifest_dir
        .parent()
        .and_then(std::path::Path::parent)
        .ok_or("packages/sdk-rs must sit two levels under the workspace root")?
        .to_path_buf();
    let proto_root = workspace_root.join("proto");
    let vendor_root = manifest_dir.join("proto-vendor");

    // The three proto files the SDK consumes. We deliberately do NOT
    // compile every file under proto/kapp/v1 — only those exposing
    // services we expose as typed clients. Adding a new service here
    // is intentional (each one needs hand-written wrappers in src/).
    let proto_files: Vec<PathBuf> = ["common.proto", "auth.proto", "ktype.proto"]
        .iter()
        .map(|f| proto_root.join("kapp/v1").join(f))
        .collect();

    for f in &proto_files {
        println!("cargo:rerun-if-changed={}", f.display());
        if !f.exists() {
            return Err(format!("missing proto file: {}", f.display()).into());
        }
    }
    println!("cargo:rerun-if-changed={}", vendor_root.display());
    println!("cargo:rerun-if-changed=build.rs");

    // Compile to a FileDescriptorSet via protox, then hand off to
    // tonic-build. protox honours the same include search order as
    // protoc: include paths in order, first match wins.
    let file_descriptor_set = protox::compile(
        proto_files.iter().map(std::path::PathBuf::as_path),
        [proto_root.as_path(), vendor_root.as_path()],
    )?;

    let out_dir = PathBuf::from(env::var("OUT_DIR")?);

    tonic_build::configure()
        // The SDK is a CLIENT crate. The server traits are only
        // useful for tests that spin up an in-process Rust gRPC
        // server (see tests/common/mod.rs); we generate them so the
        // integration harness can implement AuthService /
        // KTypeService against the same proto contract the real Go
        // server implements.
        .build_client(true)
        .build_server(true)
        // Transport stubs (tonic::transport::Channel etc.) are
        // pulled in by client crates by default; emitting them here
        // is what wires the generated `*ServiceClient::new(channel)`
        // constructor.
        .build_transport(true)
        .out_dir(&out_dir)
        // `tonic::include_proto!` looks up the package by its dotted
        // name. Our package is `kapp.v1` so it lands at
        // `kapp.v1.rs` inside OUT_DIR.
        .compile_fds(file_descriptor_set)?;

    // Re-export the generated module path through `include!` in
    // src/pb.rs so consumers of the crate get a clean `kapp_sdk::pb`
    // namespace and don't need to know about OUT_DIR machinery.
    Ok(())
}
