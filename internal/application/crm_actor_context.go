package application

import "fmt"

// ErrCRMActorContextRequired 保留普通接口的校验错误兼容性，同时供批量入口区分恢复动作。
var ErrCRMActorContextRequired = fmt.Errorf("%w: CRM_AUTHORIZATION_CONTEXT_REQUIRED", ErrValidation)
