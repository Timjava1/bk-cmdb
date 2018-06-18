package command

import (
	"configcenter/src/common"
	"configcenter/src/source_controller/api/metadata"
	"configcenter/src/source_controller/common/commondata"
	"configcenter/src/source_controller/common/instdata"
	"configcenter/src/storage"
	"encoding/json"
	"fmt"
	"gopkg.in/mgo.v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func backup(db storage.DI, opt *option) error {
	dir := filepath.Dir(opt.position)
	now := time.Now().Format("2006_01_02_15_04_05")
	file := filepath.Join(dir, "backup_bk_biz_"+now+".json")
	err := export(db, &option{position: file, OwnerID: opt.OwnerID})
	if nil != err {
		return err
	}
	fmt.Println("blueking business has been backup to \033[35m" + file + "\033[0m")
	return nil
}

func importBKBiz(db storage.DI, opt *option) error {
	file, err := os.OpenFile(opt.position, os.O_RDONLY, os.ModePerm)
	if nil != err {
		return err
	}
	defer file.Close()

	tar := new(Topo)
	json.NewDecoder(file).Decode(tar)
	if nil != err {
		return err
	}

	cur, err := getBKTopo(db, opt)
	if err != nil {
		return fmt.Errorf("get src topo faile %s", err.Error())
	}

	// err = backup(db, opt)
	// if err != nil {
	// 	return fmt.Errorf("backup faile %s", err)
	// }

	//topo check
	if !compareSlice(tar.Mainline, cur.Mainline) {
		return fmt.Errorf("different topo mainline found, your expecting import topo is [%s], but the existing topo is [%s]",
			strings.Join(tar.Mainline, "->"), strings.Join(cur.Mainline, "->"))
	}

	// walk blueking biz and get difference
	ipt := newImporter(db, opt)
	ipt.walk(true, tar.BizTopo)

	// walk to create new node
	tar.BizTopo.walk(func(node *Node) error {
		if node.mark == "create" {
			fmt.Printf("--- \033[34m%s %s %+v\033[0m\n", node.mark, node.ObjID, node.Data)
			if !opt.dryrun {
				_, err := db.Insert(commondata.GetInstTableName(node.ObjID), node.Data)
				if nil != err {
					return fmt.Errorf("insert to %s, data:%+v, error: %s", node.ObjID, node.Data, err.Error())
				}
			}
		}
		if node.mark == "update" {
			// fmt.Printf("--- \033[36m%s %s %+v\033[0m\n", node.mark, node.ObjID, node.Data)
			if !opt.dryrun {
				instID, err := node.getInstID()
				if nil != err {
					return err
				}
				updateCondition := map[string]interface{}{
					instdata.GetIDNameByType(node.ObjID): instID,
				}
				err = db.UpdateByCondition(commondata.GetInstTableName(node.ObjID), node.Data, updateCondition)
				if nil != err {
					return fmt.Errorf("update to %s by %+v data:%+v, error: %s", node.ObjID, updateCondition, node.Data, err.Error())
				}
			}
		}
		return nil
	})

	// walk to delete unuse node
	for objID, sdeletes := range ipt.sdelete {
		for _, sdelete := range sdeletes {

			instID, err := getInt64(sdelete[instdata.GetIDNameByType(objID)])
			if nil != err {
				return err
			}

			cur.BizTopo.walk(func(node *Node) error {
				nodeID, err := node.getInstID()
				if nil != err {
					return err
				}
				if node.ObjID == objID && nodeID == instID {
					node.walk(func(child *Node) error {
						childID, err := child.getInstID()
						if nil != err {
							return err
						}
						deleteconition := map[string]interface{}{
							instdata.GetIDNameByType(child.ObjID): childID,
						}
						switch child.ObjID {
						case common.BKInnerObjIDApp, common.BKInnerObjIDSet, common.BKInnerObjIDModule:
						default:
							deleteconition[common.BKObjIDField] = child.ObjID
						}
						fmt.Printf("--- \033[31mdelete %s %+v by %+v\033[0m\n", child.ObjID, node.Data, deleteconition)
						if !opt.dryrun {
							err = db.DelByCondition(commondata.GetInstTableName(objID), deleteconition)
							if nil != err {
								return fmt.Errorf("delete %s by %+v, error: %s", node.ObjID, deleteconition, err.Error())
							}
						}
						return nil
					})
					return fmt.Errorf("break")
				}
				return nil
			})

		}
	}

	return importProcess(db, opt, cur, tar)
}

func importProcess(db storage.DI, opt *option, cur, tar *Topo) error {
	bizID, err := cur.BizTopo.getInstID()
	if nil != err {
		return err
	}

	curProcs := map[string]*ProcessTopo{}
	for _, topo := range cur.ProcTopos {
		curProcs[topo.Data[common.BKProcessNameField].(string)] = topo
	}
	tarProcs := map[string]*ProcessTopo{}
	for _, topo := range tar.ProcTopos {
		tarProcs[topo.Data[common.BKProcessNameField].(string)] = topo

		topo.Data[common.BKAppIDField] = bizID
		curTopo := curProcs[topo.Data[common.BKProcessNameField].(string)]
		if curTopo != nil {
			procID, err := getInt64(curTopo.Data[common.BKProcessIDField])
			if nil != err {
				return fmt.Errorf("cur process has no bk_process_id field, %s", err.Error())
			}

			topo.Data[common.BKProcessIDField] = procID
			condition := getModifyCondition(topo.Data, []string{common.BKProcessIDField})
			// fmt.Printf("--- \033[36mupdate process by %+v, data: %+v\033[0m\n", condition, topo.Data)
			if !opt.dryrun {
				err = db.UpdateByCondition(common.BKTableNameBaseProcess, topo.Data, condition)
				if nil != err {
					return fmt.Errorf("insert process data: %+v, error: %s", topo.Data, err.Error())
				}
			}

			// add missing module
			for _, modulename := range topo.Modules {
				if inSlice(modulename, curTopo.Modules) {
					continue
				}
				procmod := ProModule{}
				procmod.ModuleName = modulename
				procmod.BizID = bizID
				procmod.ProcessID = procID
				fmt.Printf("--- \033[34minsert process module data: %+v\033[0m\n", procmod)
				if !opt.dryrun {
					_, err = db.Insert(common.BKTableNameProcModule, &procmod)
					if nil != err {
						return fmt.Errorf("insert process module data: %+v, error: %s", topo.Data, err.Error())
					}
				}
			}
			// delete unused moduel map
			for _, curmodule := range curTopo.Modules {
				if inSlice(curmodule, topo.Modules) {
					continue
				}
				delcondition := map[string]interface{}{
					common.BKModuleNameField: curmodule,
					common.BKProcessIDField:  procID,
				}
				fmt.Printf("--- \033[31mdelete process module by %+v\033[0m\n", delcondition)
				if !opt.dryrun {
					err = db.DelByCondition(common.BKTableNameProcModule, delcondition)
					if nil != err {
						return fmt.Errorf("delete process module by %+v, error: %s", delcondition, err.Error())
					}
				}
			}

		} else {
			nid, err := db.GetIncID(common.BKTableNameBaseProcess)
			if nil != err {
				return fmt.Errorf("GetIncID for prcess faile, error: %s ", err.Error())
			}
			topo.Data[common.BKProcessIDField] = nid
			fmt.Printf("--- \033[34minsert process data: %+v\033[0m\n", topo.Data)
			if !opt.dryrun {
				_, err = db.Insert(common.BKTableNameBaseProcess, topo.Data)
				if nil != err {
					return fmt.Errorf("insert process data: %+v, error: %s", topo.Data, err.Error())
				}
			}
			for _, modulename := range topo.Modules {
				procmod := ProModule{}
				procmod.ModuleName = modulename
				procmod.BizID = bizID
				procmod.ProcessID = nid
				fmt.Printf("--- \033[34minsert process module data: %+v\033[0m\n", topo.Data)
				if !opt.dryrun {
					_, err = db.Insert(common.BKTableNameProcModule, &procmod)
					if nil != err {
						return fmt.Errorf("insert process module data: %+v, error: %s", topo.Data, err.Error())
					}
				}
			}
		}
	}

	// remove unused process
	for key, proc := range curProcs {
		if tarProcs[key] == nil {
			delcondition := map[string]interface{}{
				common.BKProcessIDField: proc.Data[common.BKProcessIDField],
			}
			fmt.Printf("--- \033[31mdelete process by %+v\033[0m\n", delcondition)
			if !opt.dryrun {
				err = db.DelByCondition(common.BKTableNameBaseProcess, delcondition)
				if nil != err {
					return fmt.Errorf("delete process by %+v, error: %s", delcondition, err.Error())
				}
			}
			fmt.Printf("--- \033[31mdelete process module by %+v\033[0m\n", delcondition)
			if !opt.dryrun {
				err = db.DelByCondition(common.BKTableNameProcModule, delcondition)
				if nil != err {
					return fmt.Errorf("delete process module by %+v, error: %s", delcondition, err.Error())
				}
			}
		}
	}

	return nil
}

func getModifyCondition(data map[string]interface{}, keys []string) map[string]interface{} {
	condition := map[string]interface{}{}
	for _, key := range keys {
		condition[key] = data[key]
	}
	return condition
}

var ignoreKeys = map[string]bool{
	"_id":           true,
	"create_time":   true,
	"bk_parent_id":  true,
	"default":       true,
	"bk_biz_id":     true,
	"bk_module_id":  true,
	"bk_process_id": true,
	"bk_inst_id":    true,
}

func getUpdateData(n *Node) map[string]interface{} {
	data := map[string]interface{}{}
	for key, value := range n.Data {
		if ignoreKeys[key] {
			continue
		}
		data[key] = value
	}
	return data
}

// compareSlice returns whether slice a,b exactly equal
func compareSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

type importer struct {
	screate  map[string][]*Node
	supdate  map[string][]*Node
	sdelete  map[string][]map[string]interface{}
	bizID    int64
	setID    int64
	parentID int64

	db  storage.DI
	opt *option
}

func newImporter(db storage.DI, opt *option) *importer {
	return &importer{
		screate:  map[string][]*Node{},
		supdate:  map[string][]*Node{},
		sdelete:  map[string][]map[string]interface{}{},
		bizID:    0,
		setID:    0,
		parentID: 0,

		db:  db,
		opt: opt,
	}
}

func (ipt *importer) walk(includeRoot bool, node *Node) error {
	if node.mark != "" {
		return nil
	}
	if includeRoot {
		switch node.ObjID {
		case "biz":
			condition := getModifyCondition(node.Data, []string{common.BKAppNameField})
			app := map[string]interface{}{}
			err := ipt.db.GetOneByCondition(commondata.GetInstTableName(node.ObjID), nil, condition, &app)
			if nil != err {
				return fmt.Errorf("get blueking business by %+v error: %s", condition, err.Error())
			}
			bizID, err := getInt64(app[common.BKAppIDField])
			if nil != err {
				return fmt.Errorf("get blueking bizID faile, data: %+v, error: %s", app, err.Error())
			}
			node.Data[common.BKAppIDField] = bizID
			ipt.bizID = bizID
			ipt.parentID = bizID
			node.mark = "update"
		case "set":
			node.Data[common.BKAppIDField] = ipt.bizID
			node.Data["bk_parent_id"] = ipt.parentID
			condition := getModifyCondition(node.Data, []string{common.BKSetNameField, "bk_parent_id"})
			set := map[string]interface{}{}
			err := ipt.db.GetOneByCondition(commondata.GetInstTableName(node.ObjID), nil, condition, &set)
			if nil != err && mgo.ErrNotFound != err {
				return fmt.Errorf("get set by %+v error: %s", condition, err.Error())
			}
			if mgo.ErrNotFound == err {
				node.mark = "create"
				nid, err := ipt.db.GetIncID(node.ObjID)
				if nil != err {
					return fmt.Errorf("GetIncID error: %s", err.Error())
				}
				node.Data[common.BKSetIDField] = nid
				ipt.parentID = nid
				ipt.setID = nid
			} else {
				node.mark = "update"
				setID, err := getInt64(set[common.BKSetIDField])
				if nil != err {
					return fmt.Errorf("get setID faile, data: %+v, error: %s", set, err.Error())
				}
				node.Data[common.BKSetIDField] = setID
				ipt.parentID = setID
				ipt.setID = setID
			}
		case "module":
			node.Data[common.BKAppIDField] = ipt.bizID
			node.Data[common.BKSetIDField] = ipt.setID
			node.Data["bk_parent_id"] = ipt.parentID
			condition := getModifyCondition(node.Data, []string{common.BKModuleNameField, "bk_parent_id"})
			module := map[string]interface{}{}
			err := ipt.db.GetOneByCondition(commondata.GetInstTableName(node.ObjID), nil, condition, &module)
			if nil != err && mgo.ErrNotFound != err {
				return fmt.Errorf("get module by %+v error: %s", condition, err.Error())
			}
			if mgo.ErrNotFound == err {
				node.mark = "create"
				nid, err := ipt.db.GetIncID(node.ObjID)
				if nil != err {
					return fmt.Errorf("GetIncID error: %s", err.Error())
				}
				node.Data[common.BKModuleIDField] = nid
			} else {
				node.mark = "update"
				moduleID, err := getInt64(module[common.BKModuleIDField])
				if nil != err {
					return fmt.Errorf("get moduleID faile, data: %+v, error: %s", module, err.Error())
				}
				node.Data[common.BKModuleIDField] = moduleID
			}
		default:
			node.Data["bk_parent_id"] = ipt.parentID
			condition := getModifyCondition(node.Data, []string{node.getInstNameField(), "bk_parent_id"})
			condition[common.BKObjIDField] = node.ObjID
			inst := map[string]interface{}{}
			err := ipt.db.GetOneByCondition(commondata.GetInstTableName(node.ObjID), nil, condition, &inst)
			if nil != err && mgo.ErrNotFound != err {
				return fmt.Errorf("get inst by %+v error: %s", condition, err.Error())
			}
			if mgo.ErrNotFound == err {
				node.mark = "create"
				nid, err := ipt.db.GetIncID(node.ObjID)
				if nil != err {
					return fmt.Errorf("GetIncID error: %s", err.Error())
				}
				node.Data[instdata.GetIDNameByType(node.ObjID)] = nid
				ipt.parentID = nid
			} else {
				node.mark = "update"
				instID, err := getInt64(inst[instdata.GetIDNameByType(node.ObjID)])
				if nil != err {
					return fmt.Errorf("get instID faile, data: %+v, error: %s", inst, err.Error())
				}
				node.Data[instdata.GetIDNameByType(node.ObjID)] = instID
				ipt.parentID = instID
			}
		}

		// fetch datas that should delete
		if node.ObjID != "module" {
			childtablename := commondata.GetInstTableName(node.getChildObjID())
			instID, _ := node.getInstID()
			childCondition := map[string]interface{}{
				"bk_parent_id": instID,
				node.getChilDInstNameField(): map[string]interface{}{
					"$nin": node.getChilDInstNames(),
				},
			}
			switch node.getChildObjID() {
			case common.BKInnerObjIDApp, common.BKInnerObjIDSet, common.BKInnerObjIDModule:
			default:
				childCondition[common.BKObjIDField] = node.getChildObjID()
			}
			shouldDelete := []map[string]interface{}{}
			err := ipt.db.GetMutilByCondition(childtablename, nil, childCondition, &shouldDelete, "", 0, 0)
			if nil != err {
				return fmt.Errorf("get child of %+v error: %s", childCondition, err.Error())
			}
			if len(shouldDelete) > 0 {
				ipt.sdelete[node.getChildObjID()] = append(ipt.sdelete[node.getChildObjID()], shouldDelete...)
			}
		}

		if node.mark == "create" {
			ipt.screate[node.getChildObjID()] = append(ipt.screate[node.getChildObjID()], node)
			parentID := ipt.parentID
			bizID := ipt.bizID
			setID := ipt.setID
			err := ipt.walk(false, node)
			if nil != err {
				return nil
			}
			ipt.parentID = parentID
			ipt.bizID = bizID
			ipt.setID = setID
		}
		if node.mark == "update" {
			ipt.supdate[node.getChildObjID()] = append(ipt.supdate[node.getChildObjID()], node)
		}
	}

	parentID := ipt.parentID
	bizID := ipt.bizID
	setID := ipt.setID
	for _, child := range node.Childs {
		if err := ipt.walk(true, child); nil != err {
			return err
		}
		ipt.parentID = parentID
		ipt.bizID = bizID
		ipt.setID = setID
	}

	return nil
}

// getModelAttributes returns the model attributes
func getModelAttributes(db storage.DI, opt *option, cur *Topo) (modelAttributes map[string][]metadata.ObjectAttDes, modelKeys map[string][]string, err error) {
	condition := map[string]interface{}{
		common.BKObjIDField: map[string]interface{}{
			"$in": cur.Mainline,
		},
	}

	attributes := []metadata.ObjectAttDes{}
	err = db.GetMutilByCondition("cc_ObjAttDes", nil, condition, &attributes, "", 0, 0)
	if nil != err {
		return nil, nil, fmt.Errorf("faile to getModelAttributes for %v, error: %s", cur.Mainline, err.Error())
	}

	modelAttributes = map[string][]metadata.ObjectAttDes{}
	modelKeys = map[string][]string{}
	for _, att := range attributes {
		if att.IsOnly {
			modelKeys[att.ObjectID] = append(modelKeys[att.ObjectID], att.PropertyID)
		}
		modelAttributes[att.ObjectID] = append(modelAttributes[att.ObjectID], att)
	}

	for index := range modelKeys {
		sort.Strings(modelKeys[index])
	}

	return modelAttributes, modelKeys, nil
}

func inSlice(sub string, slice []string) bool {
	for _, s := range slice {
		if s == sub {
			return true
		}
	}
	return false
}
