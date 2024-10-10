#![doc = include_str!(concat!(env!("CARGO_MANIFEST_DIR"), "/README.md"))]

mod store;
mod vm;

use std::any::Any;
use std::cell::{Cell, RefCell};
use allocator_api2::alloc::Allocator;
use ixc::SchemaValue;
use ixc_message_api::{AccountID};
use ixc_core::{Context};
use ixc_core::account_api::{create_account_raw, ROOT_ACCOUNT};
use ixc_core::handler::{HandlerAPI, Handler, ClientFactory, Client, InitMessage, HandlerClient};
use ixc_core::resource::{InitializationError, ResourceScope, Resources};
use ixc_core::routes::{Route, Router};
use ixc_hypervisor::Hypervisor;
use ixc_message_api::code::{ErrorCode, SystemCode};
use ixc_message_api::handler::{HostBackend, RawHandler};
use ixc_message_api::header::{ContextInfo};
use ixc_message_api::packet::MessagePacket;
use ixc_schema::binary::NativeBinaryCodec;
use ixc_schema::mem::MemoryManager;
use crate::store::{VersionedMultiStore};
use crate::vm::{NativeVM};

pub use ixc_core::account_api::create_account;
use ixc_core::result::ClientResult;

/// Defines a test harness for running tests against account and module implementations.
pub struct TestApp {
    hypervisor: RefCell<Hypervisor<VersionedMultiStore>>,
    native_vm: NativeVM,
    mem: MemoryManager,
    mock_id: Cell<u64>,
}

impl Default for TestApp {
    fn default() -> Self {
        let mut hypervisor: Hypervisor<VersionedMultiStore> = Default::default();
        let native_vm = NativeVM::new();
        hypervisor.register_vm("native", std::boxed::Box::new(native_vm.clone())).unwrap();
        hypervisor.set_default_vm("native").unwrap();
        let mem = MemoryManager::new();
        let mut test_app = Self {
            hypervisor: RefCell::new(hypervisor),
            native_vm,
            mem,
            mock_id: Cell::new(0),
        };
        test_app.register_handler::<DefaultAccount>().unwrap();
        test_app
    }
}

struct DefaultAccount;
struct DefaultAccountClient(AccountID);

unsafe impl Router for DefaultAccount { const SORTED_ROUTES: &'static [Route<Self>] = &[]; }

unsafe impl Resources for DefaultAccount {
    unsafe fn new(scope: &ResourceScope) -> Result<Self, InitializationError> {
        Ok(DefaultAccount {})
    }
}

impl ClientFactory for DefaultAccount {
    type Client = DefaultAccountClient;

    fn new_client(account_id: AccountID) -> Self::Client {
        DefaultAccountClient(account_id)
    }
}

impl Client for DefaultAccountClient {
    fn account_id(&self) -> AccountID {
        self.0
    }
}

impl Handler for DefaultAccount {
    const NAME: &'static str = "ixc_testing.DefaultAccount";
    type Init<'a> = CreateDefaultAccount;
}

#[derive(SchemaValue)]
#[sealed]
struct CreateDefaultAccount;

impl <'a> InitMessage<'a> for CreateDefaultAccount {
    type Handler = DefaultAccount;
    type Codec = NativeBinaryCodec;
}

impl RawHandler for DefaultAccount {
    fn handle(&self, message_packet: &mut MessagePacket, callbacks: &dyn HostBackend, allocator: &dyn Allocator) -> Result<(), ErrorCode> {
        ixc_core::routes::exec_route(self, message_packet, callbacks, allocator)
    }
}

impl TestApp {
    /// Registers a handler with the test harness so that accounts backed by this handler can be created.
    pub fn register_handler<H: Handler>(&mut self) -> core::result::Result<(), InitializationError> {
        let scope = ResourceScope::default();
        unsafe { self.native_vm.register_handler(H::NAME, Box::new(H::new(&scope)?)); }
        Ok(())
    }
    /// Creates a new random client account that can be used in calls.
    pub fn new_client_account(&self) -> ClientResult<AccountID> {
        let mut ctx = self.client_context_for(ROOT_ACCOUNT);
        let client = create_account(&mut ctx, CreateDefaultAccount)?;
        Ok(client.0)
    }

    /// Creates a new random client account that can be used in calls and wraps it in a context.
    pub fn new_client_context(&self) -> ClientResult<Context> {
        let account_id = self.new_client_account()?;
        Ok(self.client_context_for(account_id))
    }

    /// Creates a new client for the given account.
    pub fn client_context_for(&self, account_id: AccountID) -> Context
    {
        unsafe {
            let ctx = Context::new(ContextInfo{
                account: account_id,
                caller: account_id,
                gas_limit: 0,
            }, self);
            ctx
        }
    }

    /// Adds a mock account handler to the test harness, instantiates it as an account and returns the account ID.
    pub fn add_mock(&self, ctx: &mut Context, mock: MockHandler) -> ClientResult<AccountID> {
        let mock_id = self.mock_id.get();
        self.mock_id.set(mock_id + 1);
        let handler_id = format!("mock{}", mock_id);
        self.native_vm.register_handler(&handler_id, std::boxed::Box::new(mock));
        create_account_raw(ctx, &handler_id, &[])
    }

    /// Executes a function in the context of a handler.
    /// This provides a way for tests to peek inside and manipulate a handler's state directly.
    /// This method will panic if we can't call into the handler, but panicking is acceptable in tests.
    pub fn exec_in<HC: HandlerClient, F, R>(&self, client: &HC, f: F) -> R
    where
        F: FnOnce(&HC::Handler, &mut Context)  -> R,
    {
        // TODO lookup handler ID to make sure this is the correct handler
        let scope = ResourceScope::default();
        let h = unsafe { HC::Handler::new(&scope) }.unwrap();
        let mut ctx = self.client_context_for(client.account_id());
        f(&h, &mut ctx)
    }
}

impl HostBackend for TestApp {
    fn invoke(&self, message_packet: &mut MessagePacket, allocator: &dyn Allocator) -> Result<(), ErrorCode> {
        self.hypervisor.borrow_mut().invoke(message_packet, allocator)
    }
}

/// Defines a mock handler composed of mock handler API trait implementations.
pub struct MockHandler {
    mocks: Vec<std::boxed::Box<dyn RawHandler>>,
}

impl MockHandler {
    /// Creates a new mock handler.
    pub fn new() -> Self {
        MockHandler {
            mocks: Vec::new(),
        }
    }

    /// Adds a mock handler API trait implementation to the mock handler.
    pub fn add_handler<T: RawHandler + ?Sized + 'static>(&mut self, mock: std::boxed::Box<T>) {
        self.mocks.push(std::boxed::Box::new(MockWrapper::<T>(mock)));
    }
}

impl RawHandler for MockHandler {
    fn handle(&self, message_packet: &mut MessagePacket, callbacks: &dyn HostBackend, allocator: &dyn Allocator) -> Result<(), ErrorCode> {
        for mock in &self.mocks {
            let res = mock.handle(message_packet, callbacks, allocator);
            match res {
                Err(ErrorCode::SystemCode(SystemCode::MessageNotHandled)) => continue,
                _ => return res
            }
        }
        Err(ErrorCode::SystemCode(SystemCode::MessageNotHandled))
    }
}

struct MockWrapper<T: RawHandler + ?Sized>(std::boxed::Box<T>);
impl <T: RawHandler + ?Sized> RawHandler for MockWrapper<T> {
    fn handle(&self, message_packet: &mut MessagePacket, callbacks: &dyn HostBackend, allocator: &dyn Allocator) -> Result<(), ErrorCode> {
        self.0.handle(message_packet, callbacks, allocator)
    }
}
