package connector

import (
	"encoding/json"
	"errors"
	"fmt"
	v1 "github.com/gemfire/geode-go-client/protobuf/v1"
	"github.com/gemfire/geode-go-client/query"
	"github.com/golang/protobuf/proto"
	"io"
	"net"
	"reflect"
)

//go:generate protoc --proto_path=$GEODE_CHECKOUT/geode-protobuf-messages/src/main/proto --go_out=../protobuf protocolVersion.proto
//go:generate protoc --proto_path=$GEODE_CHECKOUT/geode-protobuf-messages/src/main/proto --go_out=../protobuf v1/basicTypes.proto v1/clientProtocol.proto v1/connection_API.proto v1/locator_API.proto v1/region_API.proto v1/function_API.proto

// A Protobuf connector provides the low-level interface between a Client and the backend Geode servers.
// It should not be used directly; rather the Client API should be used.
type Protobuf struct {
	pool *Pool
}

const MAJOR_VERSION uint32 = 1
const MINOR_VERSION uint32 = 1

type RetryableError struct {
	Err error
}

func (e *RetryableError) Error() string {
	return e.Err.Error()
}

func NewConnector(pool *Pool) *Protobuf {
	return &Protobuf{
		pool: pool,
	}
}

func (this *Protobuf) Put(region string, k, v interface{}) (err error) {
	key, err := EncodeValue(k)
	if err != nil {
		return err
	}

	value, err := EncodeValue(v)
	if err != nil {
		return err
	}

	put := &v1.Message{
		MessageType: &v1.Message_PutRequest{
			PutRequest: &v1.PutRequest{
				RegionName: region,
				Entry: &v1.Entry{
					Key:   key,
					Value: value,
				},
			},
		},
	}

	_, err = this.doOperation(put)
	if err != nil {
		return err
	}

	return nil
}

func (this *Protobuf) PutIfAbsent(region string, k, v interface{}) (err error) {
	key, err := EncodeValue(k)
	if err != nil {
		return err
	}

	value, err := EncodeValue(v)
	if err != nil {
		return err
	}

	put := &v1.Message{
		MessageType: &v1.Message_PutIfAbsentRequest{
			PutIfAbsentRequest: &v1.PutIfAbsentRequest{
				RegionName: region,
				Entry: &v1.Entry{
					Key:   key,
					Value: value,
				},
			},
		},
	}

	_, err = this.doOperation(put)
	if err != nil {
		return err
	}

	return nil
}

func (this *Protobuf) Get(region string, k interface{}, value interface{}) (interface{}, error) {
	key, err := EncodeValue(k)
	if err != nil {
		return nil, err
	}

	get := &v1.Message{
		MessageType: &v1.Message_GetRequest{
			GetRequest: &v1.GetRequest{
				RegionName: region,
				Key:        key,
			},
		},
	}

	response, err := this.doOperation(get)
	if err != nil {
		return nil, err
	}

	v := response.GetGetResponse().GetResult()

	decoded, err := DecodeValue(v, value)
	if err != nil {
		return nil, err
	}

	return decoded, nil
}

func (this *Protobuf) GetAll(region string, keys interface{}) (map[interface{}]interface{}, map[interface{}]error, error) {
	keySlice := reflect.ValueOf(keys)
	if keySlice.Kind() != reflect.Slice && keySlice.Kind() != reflect.Array {
		return nil, nil, errors.New("keys must be a slice or array")
	}

	encodedKeys := make([]*v1.EncodedValue, 0, keySlice.Len())
	for i := 0; i < keySlice.Len(); i++ {
		key, err := EncodeValue(keySlice.Index(i).Interface())
		if err != nil {
			return nil, nil, err
		}

		encodedKeys = append(encodedKeys, key)
	}

	getAll := &v1.Message{
		MessageType: &v1.Message_GetAllRequest{
			GetAllRequest: &v1.GetAllRequest{
				RegionName:  region,
				Key:         encodedKeys,
				CallbackArg: nil,
			},
		},
	}

	response, err := this.doOperation(getAll)
	if err != nil {
		return nil, nil, err
	}

	decodedEntries := make(map[interface{}]interface{})
	decodedFailures := make(map[interface{}]error)

	for _, entry := range response.GetGetAllResponse().Entries {
		key, err := DecodeValue(entry.Key, nil)
		if err != nil {
			return nil, nil, errors.New(fmt.Sprintf("unable to decode GetAll response key: %s", err.Error()))
		}

		value, err := DecodeValue(entry.Value, nil)
		if err != nil {
			decodedFailures[key] = errors.New(fmt.Sprintf("unable to decode GetAll value for key: %v: %s", key, err.Error()))
			continue
		}

		decodedEntries[key] = value
	}

	for _, failure := range response.GetGetAllResponse().Failures {
		key, err := DecodeValue(failure.Key, nil)
		if err != nil {
			return nil, nil, errors.New(fmt.Sprintf("unable to decode GetAll failure response for key: %v: %s", failure.Key, err.Error()))
		}

		decodedFailures[key] = errors.New(fmt.Sprintf("%s (%d)", failure.Error.Message, failure.Error.ErrorCode))
	}

	if len(decodedFailures) == 0 {
		return decodedEntries, nil, nil
	}

	return decodedEntries, decodedFailures, nil
}

func (this *Protobuf) PutAll(region string, entries interface{}) (map[interface{}]error, error) {
	// Check if we have a map
	entriesMap := reflect.ValueOf(entries)
	if entriesMap.Kind() != reflect.Map {
		return nil, errors.New("entries must be a map")
	}

	encodedEntries := make([]*v1.Entry, 0)

	for _, k := range entriesMap.MapKeys() {
		key, err := EncodeValue(k.Interface())
		if err != nil {
			return nil, err
		}

		value, err := EncodeValue(entriesMap.MapIndex(k).Interface())
		if err != nil {
			return nil, err
		}

		e := &v1.Entry{
			Key:   key,
			Value: value,
		}

		encodedEntries = append(encodedEntries, e)
	}

	putAll := &v1.Message{
		MessageType: &v1.Message_PutAllRequest{
			PutAllRequest: &v1.PutAllRequest{
				RegionName: region,
				Entry:      encodedEntries,
			},
		},
	}

	r, err := this.doOperation(putAll)
	if err != nil {
		return nil, err
	}

	response := r.GetPutAllResponse()
	failures := make(map[interface{}]error)
	for _, k := range response.GetFailedKeys() {
		key, err := DecodeValue(k.Key, nil)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("unable to decode failed PutAll response key: %s", err.Error()))
		}

		failures[key] = errors.New(fmt.Sprintf("%s (%d)", k.GetError().Message, k.GetError().ErrorCode))
	}

	if len(failures) == 0 {
		return nil, nil
	}

	return failures, nil
}

func (this *Protobuf) Remove(region string, k interface{}) error {
	key, err := EncodeValue(k)
	if err != nil {
		return err
	}

	remove := &v1.Message{
		MessageType: &v1.Message_RemoveRequest{
			RemoveRequest: &v1.RemoveRequest{
				RegionName: region,
				Key:        key,
			},
		},
	}

	_, err = this.doOperation(remove)

	return err
}

func (this *Protobuf) Size(r string) (int32, error) {
	request := &v1.Message{
		MessageType: &v1.Message_GetSizeRequest{
			GetSizeRequest: &v1.GetSizeRequest{
				RegionName: r,
			},
		},
	}

	response, err := this.doOperation(request)
	if err != nil {
		return 0, err
	}

	size := response.GetGetSizeResponse().GetSize()

	return size, nil
}

func (this *Protobuf) ExecuteOnRegion(functionId, region string, functionArgs interface{}, keyFilter []interface{}) ([]interface{}, error) {
	args, err := EncodeValue(functionArgs)
	if err != nil {
		return nil, err
	}

	request := &v1.Message{
		MessageType: &v1.Message_ExecuteFunctionOnRegionRequest{
			ExecuteFunctionOnRegionRequest: &v1.ExecuteFunctionOnRegionRequest{
				FunctionID: functionId,
				Region:     region,
				Arguments:  args,
			},
		},
	}

	response, err := this.doOperation(request)
	if err != nil {
		return nil, err
	}

	results := response.GetExecuteFunctionOnRegionResponse().GetResults()
	return decodedFunctionResults(results)
}

func (this *Protobuf) ExecuteOnMembers(functionId string, members []string, functionArgs interface{}) ([]interface{}, error) {
	args, err := EncodeValue(functionArgs)
	if err != nil {
		return nil, err
	}

	request := &v1.Message{
		MessageType: &v1.Message_ExecuteFunctionOnMemberRequest{
			ExecuteFunctionOnMemberRequest: &v1.ExecuteFunctionOnMemberRequest{
				FunctionID: functionId,
				MemberName: members,
				Arguments:  args,
			},
		},
	}

	response, err := this.doOperation(request)
	if err != nil {
		return nil, err
	}

	results := response.GetExecuteFunctionOnMemberResponse().GetResults()
	return decodedFunctionResults(results)
}

func (this *Protobuf) ExecuteOnGroups(functionId string, groups []string, functionArgs interface{}) ([]interface{}, error) {
	args, err := EncodeValue(functionArgs)
	if err != nil {
		return nil, err
	}

	request := &v1.Message{
		MessageType: &v1.Message_ExecuteFunctionOnGroupRequest{
			ExecuteFunctionOnGroupRequest: &v1.ExecuteFunctionOnGroupRequest{
				FunctionID: functionId,
				GroupName:  groups,
				Arguments:  args,
			},
		},
	}

	response, err := this.doOperation(request)
	if err != nil {
		return nil, err
	}

	results := response.GetExecuteFunctionOnGroupResponse().GetResults()
	return decodedFunctionResults(results)
}

func (this *Protobuf) QuerySingleResult(query *query.Query) (interface{}, error) {
	response, err := this.doQuery(query.QueryString, query.BindParameters)
	if err != nil {
		return nil, err
	}

	ref := cloneStruct(query.Reference)
	result, err := DecodeValue(response.GetOqlQueryResponse().GetSingleResult(), ref)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("unable to decode query result: %s", err.Error()))
	}

	return result, nil
}

func (this *Protobuf) QueryListResult(query *query.Query) ([]interface{}, error) {
	response, err := this.doQuery(query.QueryString, query.BindParameters)
	if err != nil {
		return nil, err
	}

	// Build up a slice of results
	encodedResultList := response.GetOqlQueryResponse().GetListResult().GetElement()
	results := make([]interface{}, len(encodedResultList))

	for i, v := range encodedResultList {
		ref := cloneStruct(query.Reference)
		val, err := DecodeValue(v, ref)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("unable to decode query result: %s", err.Error()))
		}
		results[i] = val
	}

	return results, nil
}

func (this *Protobuf) QueryTableResult(query *query.Query) (map[string][]interface{}, error) {
	response, err := this.doQuery(query.QueryString, query.BindParameters)
	if err != nil {
		return nil, err
	}

	// Build up a slice of results
	table := response.GetOqlQueryResponse().GetTableResult()
	columns := table.GetFieldName()
	valueList := table.GetRow()
	results := make(map[string][]interface{}, len(columns))

	for i, columnName := range columns {
		ref := cloneStruct(query.Reference)
		val, err := DecodeValueList(valueList[i], ref)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("unable to decode query result: %s", err.Error()))
		}
		results[columnName] = val
	}

	return results, nil
}

// Clone the struct of the passed in value and return a pointer to a new, empty instance.
// Does not clone the value itself.
func cloneStruct(i interface{}) interface{} {
	if i == nil {
		return nil
	}
	return reflect.New(reflect.Indirect(reflect.ValueOf(i)).Type()).Interface()
}

func (this *Protobuf) doQuery(query string, bindParameters []interface{}) (*v1.Message, error) {
	encodedKeys := make([]*v1.EncodedValue, 0, len(bindParameters))
	for i := 0; i < len(bindParameters); i++ {
		key, err := EncodeValue(bindParameters[i])
		if err != nil {
			return nil, err
		}

		encodedKeys = append(encodedKeys, key)
	}

	request := &v1.Message{
		MessageType: &v1.Message_OqlQueryRequest{
			OqlQueryRequest: &v1.OQLQueryRequest{
				Query: query,
				BindParameter: encodedKeys,
			},
		},
	}

	response, err := this.doOperation(request)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func decodedFunctionResults(results []*v1.EncodedValue) ([]interface{}, error) {
	decodedEntries := make([]interface{}, len(results))

	for i, entry := range results {
		value, err := DecodeValue(entry, nil)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("unable to decode function result value: %s", err.Error()))
		}

		decodedEntries[i] = value
	}

	return decodedEntries, nil
}

func (this *Protobuf) doOperation(request *v1.Message) (*v1.Message, error) {
	gConn, err := this.pool.GetConnection()
	if err != nil {
		return nil, err
	}
	defer this.pool.ReturnConnection(gConn)

	message, err := doOperationWithConnection(gConn.rawConn, request)
	if err != nil {
		this.pool.DiscardConnection(gConn)
	}

	if _, ok := err.(*RetryableError); ok {
		return this.doOperation(request)
	} else if err != nil {
		return nil, err
	}

	return message, nil
}

func doOperationWithConnection(connection net.Conn, request *v1.Message) (*v1.Message, error) {
	err := writeMessage(connection, request)
	if err != nil {
		return nil, err
	}

	// In the event that the client connection has timed out, it would have been closed by the server.
	// This results in a FIN being sent to the client, however the prior write may appear to have succeeded
	// even in light of the server side of the connection being closed. It is only on a subsequent read
	// that an error will be detected. See Stevens pg 132, Section 5.13 SIGPIPE signal.
	response, err := readResponse(connection)
	if err != nil {
		if err.Error() == "EOF" {
			return nil, &RetryableError{err}
		}
		return nil, err
	}

	if x := response.GetErrorResponse(); x != nil {
		return nil, errors.New(fmt.Sprintf("%s (%d)", x.GetError().Message, x.GetError().ErrorCode))
	}

	return response, nil
}

func writeMessage(connection net.Conn, message proto.Message) (err error) {
	p := proto.NewBuffer(nil)
	err = p.EncodeMessage(message)
	if err != nil {
		return err
	}

	_, err = connection.Write(p.Bytes())
	if err != nil {
		switch nerr := err.(type) {
		case *net.OpError:
			if nerr.Op == "write" {
				return &RetryableError{err}
			}
		}
		return err
	}

	return nil
}

func readResponse(connection net.Conn) (*v1.Message, error) {
	data, err := readRawMessage(connection)
	if err != nil {
		return nil, err
	}

	p := proto.NewBuffer(data)
	response := &v1.Message{}

	if err := p.DecodeMessage(response); err != nil {
		return nil, err
	}

	return response, nil
}

func readRawMessage(connection net.Conn) ([]byte, error) {
	data := make([]byte, 4096)
	bytesRead, err := connection.Read(data)
	if err != nil {
		return nil, err
	}

	// Get the length of the message
	m, n := proto.DecodeVarint(data)
	messageLength := int(m) + n

	if messageLength > len(data) {
		t := make([]byte, len(data), messageLength)
		copy(t, data)
		data = t
	}

	for bytesRead < messageLength {
		n, err := io.ReadFull(connection, data[bytesRead:messageLength])
		if err != nil {
			return nil, err
		}

		bytesRead += n
	}

	return data[0:bytesRead], nil
}

func EncodeValue(val interface{}) (*v1.EncodedValue, error) {
	ev := &v1.EncodedValue{}

	switch k := val.(type) {
	case int:
		ev.Value = &v1.EncodedValue_IntResult{int32(k)}
	case int16:
		ev.Value = &v1.EncodedValue_ShortResult{int32(k)}
	case int32:
		ev.Value = &v1.EncodedValue_IntResult{k}
	case int64:
		ev.Value = &v1.EncodedValue_LongResult{k}
	case byte:
		ev.Value = &v1.EncodedValue_ByteResult{int32(k)}
	case bool:
		ev.Value = &v1.EncodedValue_BooleanResult{k}
	case float64:
		ev.Value = &v1.EncodedValue_DoubleResult{k}
	case float32:
		ev.Value = &v1.EncodedValue_FloatResult{k}
	case []byte:
		ev.Value = &v1.EncodedValue_BinaryResult{k}
	case string:
		ev.Value = &v1.EncodedValue_StringResult{k}
	default:
		// <nil> is not a type
		if k == nil {
			ev.Value = &v1.EncodedValue_NullResult{}
		} else {
			// Assume we have some struct and want to turn it into JSON
			j, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			ev.Value = &v1.EncodedValue_JsonObjectResult{string(j)}
		}
	}

	return ev, nil
}

func EncodeList(list interface{}) ([]*v1.EncodedValue, error) {
	listSlice := reflect.ValueOf(list)
	if listSlice.Kind() != reflect.Slice && listSlice.Kind() != reflect.Array {
		return nil, errors.New("argument must be a slice or array")
	}

	encodedEntries := make([]*v1.EncodedValue, 0, listSlice.Len())
	for i := 0; i < listSlice.Len(); i++ {
		key, err := EncodeValue(listSlice.Index(i).Interface())
		if err != nil {
			return nil, err
		}

		encodedEntries = append(encodedEntries, key)
	}

	return encodedEntries, nil
}

func EncodeValueList(list interface{}) (*v1.EncodedValueList, error) {
	encodedList, err := EncodeList(list)
	if err != nil {
		return nil, err
	}

	return &v1.EncodedValueList{Element: encodedList}, nil
}

func EncodeTable(table map[string][]interface{}) (*v1.Table, error) {
	columnNames := make([]string, len(table))
	columns := make([]*v1.EncodedValueList, len(table))

	idx := 0
	for k, v := range table {
		columnNames[idx] = k
		list, err := EncodeValueList(v)
		if err != nil {
			return nil, err
		}
		columns[idx] = list
		idx += 1
	}

	result := &v1.Table{
		FieldName: columnNames,
		Row:       columns,
	}

	return result, nil
}

func DecodeValue(value *v1.EncodedValue, ref interface{}) (interface{}, error) {
	var decodedValue interface{}

	switch v := value.GetValue().(type) {
	case *v1.EncodedValue_IntResult:
		decodedValue = v.IntResult
	case *v1.EncodedValue_ShortResult:
		decodedValue = v.ShortResult
	case *v1.EncodedValue_LongResult:
		decodedValue = v.LongResult
	case *v1.EncodedValue_ByteResult:
		// Protobuf seems to transmit bytes as int32
		decodedValue = uint8(v.ByteResult)
	case *v1.EncodedValue_BooleanResult:
		decodedValue = v.BooleanResult
	case *v1.EncodedValue_DoubleResult:
		decodedValue = v.DoubleResult
	case *v1.EncodedValue_FloatResult:
		decodedValue = v.FloatResult
	case *v1.EncodedValue_BinaryResult:
		decodedValue = v.BinaryResult
	case *v1.EncodedValue_StringResult:
		decodedValue = v.StringResult
	case *v1.EncodedValue_JsonObjectResult:
		err := json.Unmarshal([]byte(v.JsonObjectResult), ref)
		if err != nil {
			return nil, err
		}
		decodedValue = ref
	case *v1.EncodedValue_NullResult, nil:
		decodedValue = nil
	default:
		return nil, errors.New(fmt.Sprintf("unable to decode type: %T", v))
	}

	return decodedValue, nil
}

func DecodeValueList(list *v1.EncodedValueList, ref interface{}) ([]interface{}, error) {
	decodedValueList := make([]interface{}, len(list.GetElement()))

	for i, v := range list.GetElement() {
		val, err := DecodeValue(v, ref)
		if err != nil {
			return nil, err
		}

		decodedValueList[i] = val
	}

	return decodedValueList, nil
}
