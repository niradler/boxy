#[cfg(test)]
mod tests {
    // Note: these tests use a mock SandboxManager that does not require /dev/kvm.
    // Real VM tests require a KVM-enabled Linux host.

    #[derive(Clone)]
    struct MockSandboxManager {
        sandboxes: std::sync::Arc<dashmap::DashMap<String, ()>>,
    }

    impl MockSandboxManager {
        fn new() -> Self {
            Self { sandboxes: std::sync::Arc::new(dashmap::DashMap::new()) }
        }

        fn create(&self, id: &str) -> Result<(), String> {
            if self.sandboxes.contains_key(id) {
                return Err(format!("already exists: {id}"));
            }
            self.sandboxes.insert(id.to_string(), ());
            Ok(())
        }

        fn delete(&self, id: &str) -> bool {
            self.sandboxes.remove(id).is_some()
        }
    }

    #[test]
    fn test_mock_create_and_delete() {
        let mgr = MockSandboxManager::new();
        assert!(mgr.create("sandbox-1").is_ok());
        assert!(mgr.create("sandbox-1").is_err()); // duplicate
        assert!(mgr.delete("sandbox-1"));
        assert!(!mgr.delete("sandbox-1")); // gone
    }
}
