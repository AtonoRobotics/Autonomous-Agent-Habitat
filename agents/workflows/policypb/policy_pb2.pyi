from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class DecideRequest(_message.Message):
    __slots__ = ("operation_id", "payload_json", "reversibility")
    OPERATION_ID_FIELD_NUMBER: _ClassVar[int]
    PAYLOAD_JSON_FIELD_NUMBER: _ClassVar[int]
    REVERSIBILITY_FIELD_NUMBER: _ClassVar[int]
    operation_id: str
    payload_json: str
    reversibility: str
    def __init__(self, operation_id: _Optional[str] = ..., payload_json: _Optional[str] = ..., reversibility: _Optional[str] = ...) -> None: ...

class GetDecisionRequest(_message.Message):
    __slots__ = ("id",)
    ID_FIELD_NUMBER: _ClassVar[int]
    id: str
    def __init__(self, id: _Optional[str] = ...) -> None: ...

class ConsumeRequest(_message.Message):
    __slots__ = ("decision_id", "payload_json")
    DECISION_ID_FIELD_NUMBER: _ClassVar[int]
    PAYLOAD_JSON_FIELD_NUMBER: _ClassVar[int]
    decision_id: str
    payload_json: str
    def __init__(self, decision_id: _Optional[str] = ..., payload_json: _Optional[str] = ...) -> None: ...

class ConsumeResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ListPendingApprovalsRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ListPendingApprovalsResponse(_message.Message):
    __slots__ = ("approvals",)
    APPROVALS_FIELD_NUMBER: _ClassVar[int]
    approvals: _containers.RepeatedCompositeFieldContainer[ApprovalRequest]
    def __init__(self, approvals: _Optional[_Iterable[_Union[ApprovalRequest, _Mapping]]] = ...) -> None: ...

class GetApprovalRequestRequest(_message.Message):
    __slots__ = ("id",)
    ID_FIELD_NUMBER: _ClassVar[int]
    id: str
    def __init__(self, id: _Optional[str] = ...) -> None: ...

class ResolveApprovalRequest(_message.Message):
    __slots__ = ("approval_id", "resolved_by", "reason")
    APPROVAL_ID_FIELD_NUMBER: _ClassVar[int]
    RESOLVED_BY_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    approval_id: str
    resolved_by: str
    reason: str
    def __init__(self, approval_id: _Optional[str] = ..., resolved_by: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...

class Decision(_message.Message):
    __slots__ = ("id", "operation_id", "action_digest", "policy_id", "policy_version", "result", "reason_codes", "approval_request_id", "decided_at", "expires_at", "consumed_at")
    ID_FIELD_NUMBER: _ClassVar[int]
    OPERATION_ID_FIELD_NUMBER: _ClassVar[int]
    ACTION_DIGEST_FIELD_NUMBER: _ClassVar[int]
    POLICY_ID_FIELD_NUMBER: _ClassVar[int]
    POLICY_VERSION_FIELD_NUMBER: _ClassVar[int]
    RESULT_FIELD_NUMBER: _ClassVar[int]
    REASON_CODES_FIELD_NUMBER: _ClassVar[int]
    APPROVAL_REQUEST_ID_FIELD_NUMBER: _ClassVar[int]
    DECIDED_AT_FIELD_NUMBER: _ClassVar[int]
    EXPIRES_AT_FIELD_NUMBER: _ClassVar[int]
    CONSUMED_AT_FIELD_NUMBER: _ClassVar[int]
    id: str
    operation_id: str
    action_digest: str
    policy_id: str
    policy_version: str
    result: str
    reason_codes: _containers.RepeatedScalarFieldContainer[str]
    approval_request_id: str
    decided_at: str
    expires_at: str
    consumed_at: str
    def __init__(self, id: _Optional[str] = ..., operation_id: _Optional[str] = ..., action_digest: _Optional[str] = ..., policy_id: _Optional[str] = ..., policy_version: _Optional[str] = ..., result: _Optional[str] = ..., reason_codes: _Optional[_Iterable[str]] = ..., approval_request_id: _Optional[str] = ..., decided_at: _Optional[str] = ..., expires_at: _Optional[str] = ..., consumed_at: _Optional[str] = ...) -> None: ...

class ApprovalRequest(_message.Message):
    __slots__ = ("id", "decision_id", "status", "resolved_by", "resolved_at", "reason")
    ID_FIELD_NUMBER: _ClassVar[int]
    DECISION_ID_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    RESOLVED_BY_FIELD_NUMBER: _ClassVar[int]
    RESOLVED_AT_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    id: str
    decision_id: str
    status: str
    resolved_by: str
    resolved_at: str
    reason: str
    def __init__(self, id: _Optional[str] = ..., decision_id: _Optional[str] = ..., status: _Optional[str] = ..., resolved_by: _Optional[str] = ..., resolved_at: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...
